package services

import "github.com/urfave/cli"

const (
	inputFlag       = "input"
	outputFlag      = "output"
	infoHashFlag    = "info-hash"
	filePathFlag    = "file-path"
	originPathFlag  = "origin-path"
	UseSnapshotFlag = "use-snapshot"
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
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   infoHashFlag,
		EnvVar: "INFO_HASH",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   filePathFlag,
		EnvVar: "FILE_PATH",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   originPathFlag,
		EnvVar: "ORIGIN_PATH",
	})
	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:   UseSnapshotFlag,
		EnvVar: "USE_SNAPSHOT",
	})
}
