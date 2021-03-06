package empire

import (
	"io"

	"github.com/remind101/kinesumer"
)

type LogsStreamer interface {
	StreamLogs(*App, io.Writer) error
}

type nullLogsStreamer struct{}

func (s *nullLogsStreamer) StreamLogs(app *App, w io.Writer) error {
	io.WriteString(w, "Logs are disabled\n")
	return nil
}

type kinesisLogsStreamer struct{}

func (s *kinesisLogsStreamer) StreamLogs(app *App, w io.Writer) error {
	k, err := kinesumer.NewDefault(app.ID)
	if err != nil {
		return err
	}

	_, err = k.Begin()
	if err != nil {
		return err
	}
	defer k.End()

	for {
		rec := <-k.Records()
		msg := append(rec.Data(), '\n')
		if _, err := w.Write(msg); err != nil {
			return err
		}
	}
}
