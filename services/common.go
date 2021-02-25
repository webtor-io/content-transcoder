package services

import "github.com/urfave/cli"

const (
	inputFlag  = "input"
	outputFlag = "output"
)

func RegisterCommonFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:     inputFlag + ", i, url",
		Usage:    "input (url)",
		EnvVar:   "INPUT, SOURCE_URL, URL",
		Required: true,
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:  outputFlag + ", o",
		Usage: "output (local path)",
		Value: "out",
	})
}
