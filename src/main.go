// Command mock-server 是配置驱动 Go Mock Server 的唯一二进制入口。
// 只提供 serve / validate / scripts 三个子命令，三者共用 --config，
// 默认值为当前目录的 server.yaml。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/controller"
	"github.com/alexazhou/llm_mock_server/internal/service"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return constants.ExitUsageError
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "scripts":
		return runScripts(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return constants.ExitOK
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return constants.ExitUsageError
	}
}

func runServe(args []string, stdout, stderr io.Writer) int {
	configPath, code := parseConfigFlag("serve", args, stderr)
	if code != constants.ExitOK {
		return code
	}
	loaded, err := service.Load(configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return constants.ExitConfigError
	}

	listener, err := net.Listen("tcp", loaded.Resolved.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "%s: cannot listen on %s: %v\n", configPath, loaded.Resolved.Listen, err)
		return constants.ExitConfigError
	}
	srv := &http.Server{
		Handler:           controller.NewRouter(loaded.Registry),
		ReadHeaderTimeout: loaded.Resolved.ReadHeaderTimeout,
	}
	fmt.Fprintf(stdout, "listening on %s with %d scripts\n", listener.Addr().String(), loaded.Registry.Len())
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "%s: server stopped: %v\n", configPath, err)
		return constants.ExitConfigError
	}
	return constants.ExitOK
}

func runValidate(args []string, stdout, stderr io.Writer) int {
	configPath, code := parseConfigFlag("validate", args, stderr)
	if code != constants.ExitOK {
		return code
	}
	loaded, err := service.Load(configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return constants.ExitConfigError
	}
	fmt.Fprintf(stdout, "ok: %d scripts\n", loaded.Registry.Len())
	return constants.ExitOK
}

func runScripts(args []string, stdout, stderr io.Writer) int {
	configPath, code := parseConfigFlag("scripts", args, stderr)
	if code != constants.ExitOK {
		return code
	}
	loaded, err := service.Load(configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return constants.ExitConfigError
	}
	payload, err := json.MarshalIndent(loaded.Registry.Summaries(), "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "%s: cannot encode scripts: %v\n", configPath, err)
		return constants.ExitConfigError
	}
	fmt.Fprintln(stdout, string(payload))
	return constants.ExitOK
}

// parseConfigFlag 解析 --config 参数。未知参数返回非零退出码。
func parseConfigFlag(name string, args []string, stderr io.Writer) (string, int) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", constants.DefaultConfigFile, "path to server.yaml")
	if err := fs.Parse(args); err != nil {
		return "", constants.ExitUsageError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", name, fs.Arg(0))
		return "", constants.ExitUsageError
	}
	return *configPath, constants.ExitOK
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: mock-server <serve|validate|scripts> [--config server.yaml]

  serve     load and validate all config, then start the HTTP server
  validate  load and validate all config, then exit
  scripts   validate, then print script ids and their three protocol URLs as JSON

--config defaults to server.yaml in the current directory.
`)
}
