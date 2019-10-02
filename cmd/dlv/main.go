package main

import (
	"fmt"
	"os"

	"github.com/go-delve/delve/cmd/dlv/cmds"
	"github.com/go-delve/delve/pkg/version"
	"github.com/sirupsen/logrus"
)

// Build is the git sha of this binaries build.
var Build string

func main() {
	if Build != "" {
		version.DelveVersion.Build = Build
	}
	const cgoCflagsEnv = "CGO_CFLAGS"
	if os.Getenv(cgoCflagsEnv) == "" {
		os.Setenv(cgoCflagsEnv, "-O0 -g")
	} else {
		logrus.WithFields(logrus.Fields{"layer": "dlv"}).Warnln("CGO_CFLAGS already set, Cgo code could be optimized.")
	}
	fmt.Printf("PATCH APPLIED (b)\n")
	cmds.New(false).Execute()
}
