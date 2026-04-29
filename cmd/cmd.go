package cmd

import (
	log "github.com/sirupsen/logrus"

	_ "github.com/husibo16/yunzes-node/core/imports"
	"github.com/spf13/cobra"
)

var command = &cobra.Command{
	Use: "yunzes-node",
}

func Run() {
	err := command.Execute()
	if err != nil {
		log.WithField("err", err).Error("Execute command failed")
	}
}
