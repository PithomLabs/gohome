package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/barnybug/gohome/pubsub"
	"github.com/barnybug/gohome/services"
)

var NL = []byte{'\n'}

func config(path string, filenames []string) {
	if path != "config" && !strings.HasPrefix(path, "config/") {
		fmt.Println("Path must begin with 'config'")
		return
	}

	// concatenate files together
	data := &bytes.Buffer{}
	for _, filename := range filenames {
		f, err := os.Open(filename)
		if err != nil {
			fmt.Printf("Error opening %s: %s\n", filename, err)
			return
		}
		defer f.Close()
		_, err = io.Copy(data, f)
		if err != nil {
			fmt.Printf("Error reading %s: %s\n", filename, err)
			return
		}
		if !bytes.HasSuffix(data.Bytes(), NL) {
			data.WriteByte('\n')
		}
	}

	// emit event
	ev := pubsub.NewRawEvent(path, data.Bytes())
	ev.SetRetained(true) // config messages are retained
	services.SetupBroker("cmd")
	services.Publisher.Emit(ev)
	fmt.Printf("Updated %s (%d bytes)\n", path, data.Len())
	services.Shutdown()
}
