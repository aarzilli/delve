package debuginfod

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

const debuginfodFind = "debuginfod-find"

func execFind(ctx *Context, args ...string) (string, error) {
	if _, err := exec.LookPath(debuginfodFind); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx.GetContext(), debuginfodFind, args...)
	cmd.Env = append(os.Environ(), "DEBUGINFOD_PROGRESS=yes")
	if ctx.Notify != nil {
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return "", err
		}
		go func() {
			buf := make([]byte, 1024)
			n, err := stderr.Read(buf)
			if err != nil {
				return
			}
			ctx.Notify(string(buf[:n]))
			//TODO: read from stderr pass to ctx.Notify()
		}()
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), err
}

func GetSource(ctx *Context, buildid, filename string) (string, error) {
	return execFind(ctx, "source", buildid, filename)
}

func GetDebuginfo(ctx *Context, buildid string) (string, error) {
	return execFind(ctx, "debuginfo", buildid)
}

type Context struct {
	GetContext func() context.Context
	Notify     func(string)
}
