package squash

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/cloud66/cxbuild/configuration"
)

var (
	buildVersion string
	signals      chan os.Signal
	wg           sync.WaitGroup
)

type Squasher struct {
	conf *configuration.Config
}

func (s *Squasher) shutdown(tempdir string) {
	defer wg.Done()
	<-signals
	s.conf.Logger.Debug("Removing tempdir %s\n", tempdir)
	err := os.RemoveAll(tempdir)
	if err != nil {
		s.conf.Logger.Fatal(err.Error())
	}
}

func (s *Squasher) Squash(input string, output string, tag string) error {
	var from string
	keepTemp := false

	tempdir, err := ioutil.TempDir("", "docker-squash")
	if err != nil {
		return err
	}

	if tag != "" && strings.Contains(tag, ":") {
		parts := strings.Split(tag, ":")
		if parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("Bad tag format: %s\n", tag)
		}
	}

	signals = make(chan os.Signal, 1)

	if !keepTemp {
		wg.Add(1)
		signal.Notify(signals, os.Interrupt, os.Kill, syscall.SIGTERM)
		go s.shutdown(tempdir)
	}

	export, err := LoadExport(s.conf, input, tempdir)
	if err != nil {
		return err
	}

	// Export may have multiple branches with the same parent.
	// We can't handle that currently so abort.
	for _, v := range export.Repositories {
		commits := map[string]string{}
		for tag, commit := range *v {
			commits[commit] = tag
		}
		if len(commits) > 1 {
			return errors.New("This image is a full repository export w/ multiple images in it.  " +
				"You need to generate the export from a specific image ID or tag.")
		}

	}

	start := export.FirstSquash()
	// Can't find a previously squashed layer, use first FROM
	if start == nil {
		start = export.FirstFrom()
	}
	// Can't find a FROM, default to root
	if start == nil {
		start = export.Root()
	}

	if from != "" {

		if from == "root" {
			start = export.Root()
		} else {
			start, err = export.GetById(from)
			if err != nil {
				return err
			}
		}
	}

	if start == nil {
		return fmt.Errorf("no layer matching %s\n", from)
	}

	// extract each "layer.tar" to "layer" dir
	err = export.ExtractLayers()
	if err != nil {
		return err
	}

	// insert a new layer after our squash point
	newEntry, err := export.InsertLayer(start.LayerConfig.Id)
	if err != nil {
		return err
	}

	s.conf.Logger.Debug("Inserted new layer %s after %s\n", newEntry.LayerConfig.Id[0:12],
		newEntry.LayerConfig.Parent[0:12])

	e := export.Root()
	for {
		if e == nil {
			break
		}
		cmd := strings.Join(e.LayerConfig.ContainerConfig().Cmd, " ")
		if len(cmd) > 60 {
			cmd = cmd[:60]
		}

		if e.LayerConfig.Id == newEntry.LayerConfig.Id {
			s.conf.Logger.Debug("  -> %s %s\n", e.LayerConfig.Id[0:12], cmd)
		} else {
			s.conf.Logger.Debug("  -  %s %s\n", e.LayerConfig.Id[0:12], cmd)
		}
		e = export.ChildOf(e.LayerConfig.Id)
	}

	// squash all later layers into our new layer
	err = export.SquashLayers(newEntry, newEntry)
	if err != nil {
		return err
	}

	s.conf.Logger.Debug("Tarring up squashed layer %s\n", newEntry.LayerConfig.Id[:12])
	// create a layer.tar from our squashed layer
	err = newEntry.TarLayer()
	if err != nil {
		return err
	}

	s.conf.Logger.Debug("Removing extracted layers\n")
	// remove our expanded "layer" dirs
	err = export.RemoveExtractedLayers()
	if err != nil {
		return err
	}

	if tag != "" {
		tagPart := "latest"
		repoPart := tag
		parts := strings.Split(tag, ":")
		if len(parts) > 1 {
			repoPart = parts[0]
			tagPart = parts[1]
		}
		tagInfo := TagInfo{}
		layer := export.LastChild()

		tagInfo[tagPart] = layer.LayerConfig.Id
		export.Repositories[repoPart] = &tagInfo

		s.conf.Logger.Debug("Tagging %s as %s:%s\n", layer.LayerConfig.Id[0:12], repoPart, tagPart)
		err := export.WriteRepositoriesJson()
		if err != nil {
			return err
		}
	}

	ow := os.Stdout
	if output != "" {
		var err error
		ow, err = os.Create(output)
		if err != nil {
			return err
		}
		s.conf.Logger.Debug("Tarring new image to %s\n", output)
	} else {
		s.conf.Logger.Debug("Tarring new image to STDOUT\n")
	}
	// bundle up the new image
	err = export.TarLayers(ow)
	if err != nil {
		return err
	}

	s.conf.Logger.Debug("Done. New image created.")
	// print our new history
	export.PrintHistory()

	signals <- os.Interrupt
	wg.Wait()

	return nil
}
