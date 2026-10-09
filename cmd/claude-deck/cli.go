package main

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pomesaka/claude-deck/internal/config"
	"github.com/pomesaka/claude-deck/internal/control"
)

// cliCommands are the subcommands that talk to the running TUI over the control socket.
var cliCommands = map[string]func(args []string) (control.Request, error){
	"new":   parseNewArgs,
	"list":  parseListArgs,
	"close": parseCloseArgs,
}

// runCLI sends one subcommand to the running TUI and prints the response as JSON.
func runCLI(name string, args []string) error {
	parse := cliCommands[name]
	req, err := parse(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	resp, err := control.Call(cfg.DataDir, req)
	if err != nil {
		return err
	}
	return printJSON(cliOutput(req.Op, resp))
}

// cliOutput picks the part of the response the subcommand prints.
func cliOutput(op control.Op, resp control.Response) any {
	if op == control.OpList {
		if resp.Sessions == nil {
			return []control.SessionInfo{}
		}
		return resp.Sessions
	}
	return resp.Session
}

func parseNewArgs(args []string) (control.Request, error) {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck new [--dir DIR] [--no-workspace]")
		fmt.Fprintln(fs.Output(), "TUI で n を押したときと同じく新しいセッションを作り、JSON で返す。")
		fs.PrintDefaults()
	}
	dir := fs.String("dir", "", "セッションを起動するディレクトリ（既定: カレントディレクトリ）。jj ワークスペース内なら本体リポジトリの同じ位置に解決する")
	noWorkspace := fs.Bool("no-workspace", false, "jj ワークスペースを作らずに直接起動する（TUI の C-Enter）")
	if err := fs.Parse(args); err != nil {
		return control.Request{}, err
	}
	if fs.NArg() > 0 {
		return control.Request{}, fmt.Errorf("new: unexpected arguments: %v", fs.Args())
	}
	absDir, err := resolveDir(*dir)
	if err != nil {
		return control.Request{}, err
	}
	return control.Request{Op: control.OpNew, Dir: absDir, NoWorkspace: *noWorkspace}, nil
}

func parseListArgs(args []string) (control.Request, error) {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck list")
		fmt.Fprintln(fs.Output(), "全セッションを TUI の一覧と同じ順で JSON で返す。")
	}
	if err := fs.Parse(args); err != nil {
		return control.Request{}, err
	}
	if fs.NArg() > 0 {
		return control.Request{}, fmt.Errorf("list: unexpected arguments: %v", fs.Args())
	}
	return control.Request{Op: control.OpList}, nil
}

func parseCloseArgs(args []string) (control.Request, error) {
	fs := flag.NewFlagSet("close", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: claude-deck close <ID|NAME>")
		fmt.Fprintln(fs.Output(), "TUI で x を押したときと同じくプロセスを止めてワークスペースを消す。r で再開できる。")
	}
	if err := fs.Parse(args); err != nil {
		return control.Request{}, err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return control.Request{}, fmt.Errorf("close: specify exactly one session ID or name")
	}
	return control.Request{Op: control.OpClose, Target: fs.Arg(0)}, nil
}

func resolveDir(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getting working directory: %w", err)
		}
		return wd, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", dir, err)
	}
	return abs, nil
}

func printJSON(v any) error {
	if err := json.MarshalWrite(os.Stdout, v, jsontext.WithIndent("  ")); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	fmt.Println()
	return nil
}
