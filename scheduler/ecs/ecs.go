// Pacakge ecs provides an implementation of the Scheduler interface that uses
// Amazon EC2 Container Service.
package ecs

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ecs"
	shellwords "github.com/mattn/go-shellwords"
	"github.com/remind101/empire/pkg/arn"
	. "github.com/remind101/empire/pkg/bytesize"
	"github.com/remind101/empire/pkg/ecsutil"
	"github.com/remind101/empire/pkg/lb"
	"github.com/remind101/empire/scheduler"
	"github.com/remind101/pkg/timex"
	"golang.org/x/net/context"
)

var DefaultDelimiter = "-"

// ProcessManager is a lower level interface than Scheduler, that provides direct
// control over individual processes.
type ProcessManager interface {
	scheduler.Scaler
	scheduler.Runner

	// CreateProcess creates a process for the app.
	CreateProcess(ctx context.Context, app *scheduler.App, process *scheduler.Process) error

	// RemoveProcess removes a process for the app.
	RemoveProcess(ctx context.Context, app string, process string) error

	// Processes returns all processes for the app.
	Processes(ctx context.Context, app string) ([]*scheduler.Process, error)
}

// Scheduler is an implementation of the ServiceManager interface that
// is backed by Amazon ECS.
type Scheduler struct {
	ProcessManager

	cluster string
	ecs     *ecsutil.Client
}

// Config holds configuration for generating a new ECS backed Scheduler
// implementation.
type Config struct {
	// The ECS cluster to create services and task definitions in.
	Cluster string

	// The IAM role to use for ECS services with ELBs attached.
	ServiceRole string

	// VPC controls what subnets to attach to ELBs that are created.
	VPC string

	// The hosted zone id to create internal DNS records in
	ZoneID string

	// The ID of the security group to assign to internal load balancers.
	InternalSecurityGroupID string

	// The ID of the security group to assign to external load balancers.
	ExternalSecurityGroupID string

	// The Subnet IDs to assign when creating internal load balancers.
	InternalSubnetIDs []string

	// The Subnet IDs to assign when creating external load balancers.
	ExternalSubnetIDs []string

	// AWS configuration.
	AWS *aws.Config
}

// NewScheduler returns a new Scehduler implementation that:
//
// * Creates services with ECS.
func NewScheduler(config Config) (*Scheduler, error) {
	c := ecsutil.NewClient(config.AWS)

	// Create the ECS Scheduler
	var pm ProcessManager = &ecsProcessManager{
		cluster:     config.Cluster,
		serviceRole: config.ServiceRole,
		ecs:         c,
	}

	return &Scheduler{
		cluster:        config.Cluster,
		ProcessManager: pm,
		ecs:            c,
	}, nil
}

// NewLoadBalancedScheduler returns a new Scheduler instance that:
//
// * Creates services with ECS.
// * Creates internal or external ELBs for ECS services.
// * Creates a CNAME record in route53 under the internal TLD.
func NewLoadBalancedScheduler(config Config) (*Scheduler, error) {
	if err := validateLoadBalancedConfig(config); err != nil {
		return nil, err
	}

	c := ecsutil.NewClient(config.AWS)

	// Create the ECS Scheduler
	var pm ProcessManager = &ecsProcessManager{
		cluster:     config.Cluster,
		serviceRole: config.ServiceRole,
		ecs:         c,
	}

	// Create the ELB Manager
	elb := lb.NewELBManager(config.AWS)
	elb.InternalSecurityGroupID = config.InternalSecurityGroupID
	elb.ExternalSecurityGroupID = config.ExternalSecurityGroupID
	elb.InternalSubnetIDs = config.InternalSubnetIDs
	elb.ExternalSubnetIDs = config.ExternalSubnetIDs

	// Compose the LB Manager
	var lbm lb.Manager = elb

	n := lb.NewRoute53Nameserver(config.AWS)
	n.ZoneID = config.ZoneID

	lbm = lb.WithCNAME(lbm, n)
	lbm = lb.WithLogging(lbm)

	pm = &LBProcessManager{
		ProcessManager: pm,
		lb:             lbm,
	}

	return &Scheduler{
		cluster:        config.Cluster,
		ProcessManager: pm,
		ecs:            c,
	}, nil
}

func validateLoadBalancedConfig(c Config) error {
	r := func(n string) error {
		return errors.New(fmt.Sprintf("%s is required", n))
	}

	if c.Cluster == "" {
		return r("Cluster")
	}
	if c.ServiceRole == "" {
		return r("ServiceRole")
	}
	if c.ZoneID == "" {
		return r("ZoneID")
	}
	if c.InternalSecurityGroupID == "" {
		return r("InternalSecurityGroupID")
	}
	if c.ExternalSecurityGroupID == "" {
		return r("ExternalSecurityGroupID")
	}
	if len(c.InternalSubnetIDs) == 0 {
		return r("InternalSubnetIDs")
	}
	if len(c.ExternalSubnetIDs) == 0 {
		return r("ExternalSubnetIDs")
	}

	return nil
}

// Submit will create an ECS service for each individual process in the App. New
// task definitions will be created based on the information with each process.
//
// If the app was previously submitted with different process than what are
// provided, any process types that don't exist in the new release will be
// removed from ECS. For example, if you previously submitted an app with a
// `web` and `worker` process, then submit an app with the `web` process, the
// ECS service for the old `worker` process will be removed.
func (m *Scheduler) Submit(ctx context.Context, app *scheduler.App) error {
	processes, err := m.Processes(ctx, app.ID)
	if err != nil {
		return err
	}

	for _, p := range app.Processes {
		if err := m.CreateProcess(ctx, app, p); err != nil {
			return err
		}
	}

	toRemove := diffProcessTypes(processes, app.Processes)
	for _, p := range toRemove {
		if err := m.RemoveProcess(ctx, app.ID, p); err != nil {
			return err
		}
	}

	return nil
}

// Remove removes any ECS services that belong to this app.
func (m *Scheduler) Remove(ctx context.Context, appID string) error {
	processes, err := m.Processes(ctx, appID)
	if err != nil {
		return err
	}

	for t, _ := range processTypes(processes) {
		if err := m.RemoveProcess(ctx, appID, t); err != nil {
			return err
		}
	}

	return nil
}

// Instances returns all instances that are currently running, pending or
// draining.
func (m *Scheduler) Instances(ctx context.Context, appID string) ([]*scheduler.Instance, error) {
	var instances []*scheduler.Instance

	tasks, err := m.describeAppTasks(ctx, appID)
	if err != nil {
		return instances, err
	}

	for _, t := range tasks {
		resp, err := m.ecs.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
			TaskDefinition: t.TaskDefinitionArn,
		})
		if err != nil {
			return instances, err
		}

		id, err := arn.ResourceID(*t.TaskArn)
		if err != nil {
			return instances, err
		}

		p, err := taskDefinitionToProcess(resp.TaskDefinition)
		if err != nil {
			return instances, err
		}

		instances = append(instances, &scheduler.Instance{
			Process:   p,
			State:     safeString(t.LastStatus),
			ID:        id,
			UpdatedAt: timex.Now(),
		})
	}

	return instances, nil
}

func (m *Scheduler) describeAppTasks(ctx context.Context, appID string) ([]*ecs.Task, error) {
	resp, err := m.ecs.ListAppTasks(ctx, appID, &ecs.ListTasksInput{
		Cluster: aws.String(m.cluster),
	})
	if err != nil {
		return nil, err
	}

	if len(resp.TaskArns) == 0 {
		return []*ecs.Task{}, nil
	}

	tasks, err := m.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(m.cluster),
		Tasks:   resp.TaskArns,
	})
	return tasks.Tasks, err
}

func (m *Scheduler) Stop(ctx context.Context, instanceID string) error {
	_, err := m.ecs.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: aws.String(m.cluster),
		Task:    aws.String(instanceID),
	})
	return err
}

var _ ProcessManager = &ecsProcessManager{}

// ecsProcessManager is an implementation of the ProcessManager interface that
// creates ECS services for Processes.
type ecsProcessManager struct {
	cluster     string
	serviceRole string
	ecs         *ecsutil.Client
}

// CreateProcess creates an ECS service for the process.
func (m *ecsProcessManager) CreateProcess(ctx context.Context, app *scheduler.App, p *scheduler.Process) error {
	if _, err := m.createTaskDefinition(ctx, app, p); err != nil {
		return err
	}

	_, err := m.updateCreateService(ctx, app, p)
	return err
}

func (m *ecsProcessManager) Run(ctx context.Context, app *scheduler.App, process *scheduler.Process, in io.Reader, out io.Writer) error {
	attachment := "detached"
	if out != nil {
		attachment = "attached"
	}
	// TODO(ejholmes): Actually implement this. We could have the ECS
	// manager handle the lifecycle of "detached" processes which don't have
	// their stdin, stdout and stderr attached. It would be nice if we can
	// eventually remove the "runner" package and have both attached and
	// detached processes run via ECS.
	return fmt.Errorf("running a %s process is not implemented by the ECS manager.", attachment)
}

// createTaskDefinition creates a Task Definition in ECS for the service.
func (m *ecsProcessManager) createTaskDefinition(ctx context.Context, app *scheduler.App, process *scheduler.Process) (*ecs.TaskDefinition, error) {
	taskDef, err := taskDefinitionInput(process)
	if err != nil {
		return nil, err
	}

	resp, err := m.ecs.RegisterAppTaskDefinition(ctx, app.ID, taskDef)
	return resp.TaskDefinition, err
}

// createService creates a Service in ECS for the service.
func (m *ecsProcessManager) createService(ctx context.Context, app *scheduler.App, p *scheduler.Process) (*ecs.Service, error) {
	var role *string
	var loadBalancers []*ecs.LoadBalancer

	if p.LoadBalancer != "" {
		loadBalancers = []*ecs.LoadBalancer{
			{
				ContainerName:    aws.String(p.Type),
				ContainerPort:    p.Ports[0].Container,
				LoadBalancerName: aws.String(p.LoadBalancer),
			},
		}
		role = aws.String(m.serviceRole)
	}

	resp, err := m.ecs.CreateAppService(ctx, app.ID, &ecs.CreateServiceInput{
		Cluster:        aws.String(m.cluster),
		DesiredCount:   aws.Int64(int64(p.Instances)),
		ServiceName:    aws.String(p.Type),
		TaskDefinition: aws.String(p.Type),
		LoadBalancers:  loadBalancers,
		Role:           role,
	})
	return resp.Service, err
}

// updateService updates an existing Service in ECS.
func (m *ecsProcessManager) updateService(ctx context.Context, app *scheduler.App, p *scheduler.Process) (*ecs.Service, error) {
	resp, err := m.ecs.UpdateAppService(ctx, app.ID, &ecs.UpdateServiceInput{
		Cluster:        aws.String(m.cluster),
		DesiredCount:   aws.Int64(int64(p.Instances)),
		Service:        aws.String(p.Type),
		TaskDefinition: aws.String(p.Type),
	})

	// If the service does not exist, return nil.
	if noService(err) {
		return nil, nil
	}

	return resp.Service, err
}

// updateCreateService will perform an upsert for the service in ECS.
func (m *ecsProcessManager) updateCreateService(ctx context.Context, app *scheduler.App, p *scheduler.Process) (*ecs.Service, error) {
	s, err := m.updateService(ctx, app, p)
	if err != nil {
		return nil, err
	}

	if s != nil {
		return s, nil
	}

	return m.createService(ctx, app, p)
}

func (m *ecsProcessManager) Processes(ctx context.Context, appID string) ([]*scheduler.Process, error) {
	var processes []*scheduler.Process

	list, err := m.ecs.ListAppServices(ctx, appID, &ecs.ListServicesInput{
		Cluster: aws.String(m.cluster),
	})
	if err != nil {
		return processes, err
	}

	if len(list.ServiceArns) == 0 {
		return processes, nil
	}

	desc, err := m.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  aws.String(m.cluster),
		Services: list.ServiceArns,
	})
	if err != nil {
		return processes, err
	}

	for _, s := range desc.Services {
		resp, err := m.ecs.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
			TaskDefinition: s.TaskDefinition,
		})
		if err != nil {
			return processes, err
		}

		p, err := taskDefinitionToProcess(resp.TaskDefinition)
		if err != nil {
			return processes, err
		}

		processes = append(processes, p)
	}

	return processes, nil
}

func (m *ecsProcessManager) RemoveProcess(ctx context.Context, app string, process string) error {
	if err := m.Scale(ctx, app, process, 0); noService(err) {
		return nil
	} else if err != nil {
		return err
	}

	_, err := m.ecs.DeleteAppService(ctx, app, &ecs.DeleteServiceInput{
		Cluster: aws.String(m.cluster),
		Service: aws.String(process),
	})
	if noService(err) {
		return nil
	}

	return err
}

// Scale scales an ECS service to the desired number of instances.
func (m *ecsProcessManager) Scale(ctx context.Context, app string, process string, instances uint) error {
	_, err := m.ecs.UpdateAppService(ctx, app, &ecs.UpdateServiceInput{
		Cluster:      aws.String(m.cluster),
		DesiredCount: aws.Int64(int64(instances)),
		Service:      aws.String(process),
	})
	return err
}

// taskDefinitionInput returns an ecs.RegisterTaskDefinitionInput suitable for
// creating a task definition from a Process.
func taskDefinitionInput(p *scheduler.Process) (*ecs.RegisterTaskDefinitionInput, error) {
	args, err := shellwords.Parse(p.Command)
	if err != nil {
		return nil, err
	}

	// ecs.ContainerDefinition{Command} is expecting a []*string
	var command []*string
	for _, s := range args {
		ss := s
		command = append(command, &ss)
	}

	var environment []*ecs.KeyValuePair
	for k, v := range p.Env {
		environment = append(environment, &ecs.KeyValuePair{
			Name:  aws.String(k),
			Value: aws.String(v),
		})
	}

	var ports []*ecs.PortMapping
	for _, m := range p.Ports {
		ports = append(ports, &ecs.PortMapping{
			HostPort:      m.Host,
			ContainerPort: m.Container,
		})
	}

	return &ecs.RegisterTaskDefinitionInput{
		Family: aws.String(p.Type),
		ContainerDefinitions: []*ecs.ContainerDefinition{
			&ecs.ContainerDefinition{
				Name:         aws.String(p.Type),
				Cpu:          aws.Int64(int64(p.CPUShares)),
				Command:      command,
				Image:        aws.String(p.Image.String()),
				Essential:    aws.Bool(true),
				Memory:       aws.Int64(int64(p.MemoryLimit / MB)),
				Environment:  environment,
				PortMappings: ports,
			},
		},
	}, nil
}

func safeString(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

func noService(err error) bool {
	if err, ok := err.(awserr.Error); ok {
		if err.Message() == "Service was not ACTIVE." {
			return true
		}

		// Wat
		if err.Message() == "Could not find returned type com.amazon.madison.cmb#CMServiceNotActiveException in model" {
			return true
		}
		if err.Message() == "Could not find returned type com.amazon.madison.cmb#CMServiceNotFoundException in model" {
			return true
		}

		if err.Message() == "Service not found." {
			return true
		}
	}

	return false
}

// taskDefinitionToProcess takes an ECS Task Definition and converts it to a
// Process.
func taskDefinitionToProcess(td *ecs.TaskDefinition) (*scheduler.Process, error) {
	// If this task definition has no container definitions, then something
	// funky is up.
	if len(td.ContainerDefinitions) == 0 {
		return nil, errors.New("task definition had no container definitions")
	}

	container := td.ContainerDefinitions[0]

	var command []string
	for _, s := range container.Command {
		command = append(command, *s)
	}

	env := make(map[string]string)
	for _, kvp := range container.Environment {
		if kvp != nil {
			env[safeString(kvp.Name)] = safeString(kvp.Value)
		}
	}

	return &scheduler.Process{
		Type:        safeString(container.Name),
		Command:     strings.Join(command, " "),
		Env:         env,
		CPUShares:   uint(*container.Cpu),
		MemoryLimit: uint(*container.Memory) * MB,
	}, nil
}

func diffProcessTypes(old, new []*scheduler.Process) []string {
	var types []string

	om := processTypes(old)
	nm := processTypes(new)

	for t, _ := range om {
		if _, ok := nm[t]; !ok {
			types = append(types, t)
		}
	}

	return types
}

func processTypes(processes []*scheduler.Process) map[string]struct{} {
	m := make(map[string]struct{})

	for _, p := range processes {
		m[p.Type] = struct{}{}
	}

	return m
}
