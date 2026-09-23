package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/marcioapm/lux/internal/proto"
)

func jsonUnmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func (a *app) execCmd() *cobra.Command {
	var tty, noTTY bool
	cmd := &cobra.Command{
		Use:   "exec <run> -- command...",
		Short: "Run a command inside a Run's container",
		Long: `Run a command inside a running Run's container, as its workload user,
with its environment. Standard input, output and the exit code are the
command's. A terminal is allocated when stdin is one (-t forces it, -T turns
it off).`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctxOf(cmd)
			useTTY := (tty || term.IsTerminal(int(os.Stdin.Fd()))) && !noTTY
			ws, err := a.c.Dial(ctx, "/v1/runs/"+args[0]+"/exec")
			if err != nil {
				return err
			}
			defer ws.CloseNow()
			open := proto.StreamOpen{Command: args[1:], TTY: useTTY}
			if useTTY {
				open.Cols, open.Rows, _ = term.GetSize(int(os.Stdout.Fd()))
			}
			if err := wsWrite(ctx, ws, open); err != nil {
				return err
			}
			return a.interact(ctx, ws, useTTY)
		},
	}
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a terminal")
	cmd.Flags().BoolVarP(&noTTY, "no-tty", "T", false, "do not allocate a terminal")
	return cmd
}

func (a *app) attachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <run>",
		Short: "Attach to a generic workload's terminal",
		Long: `Attach to the terminal of a generic workload started with workload.tty:
see its output from now on and type into it. Detach with Ctrl-] (the
workload keeps running).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctxOf(cmd)
			ws, err := a.c.Dial(ctx, "/v1/runs/"+args[0]+"/attach")
			if err != nil {
				return err
			}
			defer ws.CloseNow()
			return a.interact(ctx, ws, term.IsTerminal(int(os.Stdin.Fd())))
		},
	}
}

// interact connects the terminal to a stream until it ends, and returns
// the remote exit code as the command's.
func (a *app) interact(ctx context.Context, ws *websocket.Conn, raw bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if raw {
		if st, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
			defer term.Restore(int(os.Stdin.Fd()), st)
		}
		// Window size changes follow the local terminal.
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				if c, r, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
					_ = wsWrite(ctx, ws, proto.StreamData{Rows: r, Cols: c})
				}
			}
		}()
	}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := a.stdin.Read(buf)
			if n > 0 {
				// Ctrl-] detaches from a terminal session.
				if raw && n == 1 && buf[0] == 0x1d {
					cancel()
					return
				}
				if wsWrite(ctx, ws, proto.StreamData{Data: append([]byte{}, buf[:n]...)}) != nil {
					return
				}
			}
			if err != nil {
				_ = wsWrite(ctx, ws, proto.StreamData{EOF: true})
				return
			}
		}
	}()
	for {
		var d proto.StreamData
		if err := wsRead(ctx, ws, &d); err != nil {
			if ctx.Err() != nil {
				return nil // detached
			}
			return fmt.Errorf("stream ended: %w", err)
		}
		switch {
		case d.Error != "":
			return fmt.Errorf("%s", d.Error)
		case d.ExitCode != nil:
			if *d.ExitCode != 0 {
				return exitCode(*d.ExitCode)
			}
			return nil
		case d.Channel == "stderr":
			_, _ = a.stderr.Write(d.Data)
		default:
			_, _ = a.stdout.Write(d.Data)
		}
	}
}

func (a *app) portForwardCmd() *cobra.Command {
	var address string
	cmd := &cobra.Command{
		Use:   "port-forward <run> <port-name> <local-port>",
		Short: "Forward a local port to a Run's declared port",
		Long: `Listen on a local port and tunnel each connection, through luxd, to a
port the Run declares in network.ports. Runs until interrupted.`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctxOf(cmd)
			port, err := strconv.Atoi(args[2])
			if err != nil {
				return fmt.Errorf("local port: %w", err)
			}
			// Check before listening: a wrong name or a stopped Run fails now.
			probe, err := a.c.Dial(ctx, "/v1/runs/"+args[0]+"/ports/"+args[1])
			if err != nil {
				return err
			}
			probe.CloseNow()
			ln, err := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(port)))
			if err != nil {
				return err
			}
			defer ln.Close()
			fmt.Fprintf(a.stderr, "forwarding %s → %s:%s\n", ln.Addr(), args[0], args[1])
			go func() { <-ctx.Done(); ln.Close() }()
			for {
				c, err := ln.Accept()
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
				go a.tunnel(ctx, c, "/v1/runs/"+args[0]+"/ports/"+args[1])
			}
		},
	}
	cmd.Flags().StringVar(&address, "address", "127.0.0.1", "local address to listen on")
	return cmd
}

// tunnel relays one local connection through its own stream.
func (a *app) tunnel(ctx context.Context, c net.Conn, path string) {
	defer c.Close()
	ws, err := a.c.Dial(ctx, path)
	if err != nil {
		fmt.Fprintln(a.stderr, "port-forward:", err)
		return
	}
	defer ws.CloseNow()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Once
	go func() {
		defer once.Do(cancel)
		buf := make([]byte, 32<<10)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				if wsWrite(ctx, ws, proto.StreamData{Data: append([]byte{}, buf[:n]...)}) != nil {
					return
				}
			}
			if err != nil {
				_ = wsWrite(ctx, ws, proto.StreamData{EOF: true})
				// Keep reading the other way until the remote side ends.
				<-ctx.Done()
				return
			}
		}
	}()
	for {
		var d proto.StreamData
		if err := wsRead(ctx, ws, &d); err != nil {
			return
		}
		if d.ExitCode != nil || d.Error != "" {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
			return
		}
		if _, err := c.Write(d.Data); err != nil {
			return
		}
	}
}

func wsRead(ctx context.Context, ws *websocket.Conn, v any) error {
	_, b, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func wsWrite(ctx context.Context, ws *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageText, b)
}
