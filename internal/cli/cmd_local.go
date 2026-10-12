package cli

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/usedock/docklet/internal/dockyard"
	"github.com/usedock/docklet/internal/guestagent"
	"github.com/usedock/docklet/internal/local"
)

// cmdImages lists Dockyard images, or pulls one with 'images pull <name>'.
func (a *App) cmdImages(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "pull" {
		c := a.flags("images pull", "images pull <name>")
		pos, err := c.parse(args[1:], 1, 1)
		if err != nil {
			return err
		}
		e, err := a.engine()
		if err != nil {
			return err
		}
		if err := e.Driver.Preflight(ctx); err != nil {
			return localError(err)
		}
		im, err := e.ResolveImage(ctx, pos[0])
		if err != nil {
			return localError(err)
		}
		if _, err := e.Cache.Ensure(ctx, im); err != nil {
			return localError(err)
		}
		if *c.json {
			return a.printValue(imageRow(e, im))
		}
		fmt.Fprintf(a.Stdout, "pulled %s (%s)\n", im.Name, local.HumanBytes(local.ImageSize(im)))
		return nil
	}
	c := a.flags("images", "images | images pull <name>")
	if _, err := c.parse(args, 0, 0); err != nil {
		return err
	}
	e, err := a.engine()
	if err != nil {
		return err
	}
	ix, err := e.Catalog.Index(ctx)
	if err != nil {
		if local.ErrCode(err) == "no_dockyard_release" {
			// Nothing is wrong with the machine; there is just no release to list yet.
			if *c.json {
				return a.printValue([]imageInfo{})
			}
			fmt.Fprintln(a.Stdout, "no Dockyard release pinned yet; there are no images to list")
			return nil
		}
		return localError(err)
	}
	rows := []imageInfo{}
	for _, im := range ix.Images {
		if im.Arch == e.Arch {
			rows = append(rows, imageRow(e, im))
		}
	}
	if *c.json {
		return a.printValue(rows)
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSIZE\tCACHED\tDESCRIPTION")
	for _, r := range rows {
		cached := "no"
		if r.Cached {
			cached = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, local.HumanBytes(r.SizeBytes), cached, r.Description)
	}
	return tw.Flush()
}

type imageInfo struct {
	Name        string `json:"name"`
	Arch        string `json:"arch"`
	Description string `json:"description"`
	SizeBytes   int64  `json:"sizeBytes"`
	Cached      bool   `json:"cached"`
}

func imageRow(e *local.Engine, im dockyard.Image) imageInfo {
	return imageInfo{Name: im.Name, Arch: im.Arch, Description: im.Description, SizeBytes: local.ImageSize(im), Cached: e.Cache.Cached(im)}
}

// cmdDoctor reports what local docks need and what is missing; it exits 1 when something is.
func (a *App) cmdDoctor(ctx context.Context, args []string) error {
	c := a.flags("doctor", "doctor")
	if _, err := c.parse(args, 0, 0); err != nil {
		return err
	}
	e, err := a.engine()
	if err != nil {
		return err
	}
	checks := e.Doctor(ctx)
	bad := false
	for _, ck := range checks {
		bad = bad || (!ck.OK && !ck.Optional)
	}
	if *c.json {
		if err := a.printValue(checks); err != nil {
			return err
		}
	} else {
		for _, ck := range checks {
			mark := "ok     "
			if !ck.OK {
				mark = "missing"
				if ck.Optional {
					mark = "info   "
				}
			}
			fmt.Fprintf(a.Stdout, "[%s] %-14s %s\n", mark, ck.Name, ck.Detail)
			if ck.Fix != "" {
				fmt.Fprintf(a.Stdout, "          fix: %s\n", ck.Fix)
			}
		}
		if !bad {
			fmt.Fprintln(a.Stdout, "everything local docks need is in place; try: dock new")
		}
	}
	if bad {
		return &exitError{code: 1}
	}
	return nil
}

// localShell runs an interactive shell in a local dock through the guest agent pty.
func (a *App) localShell(ctx context.Context, e *local.Engine, ref string) error {
	fd := int(os.Stdin.Fd())
	out := int(os.Stdout.Fd())
	cols, rows := 80, 24
	tty := a.StdinIsTTY
	if tty {
		if w, h, err := term.GetSize(out); err == nil {
			cols, rows = w, h
		}
	}
	t, err := e.OpenTerminal(ctx, ref, cols, rows)
	if err != nil {
		return localError(err)
	}
	defer t.Close()
	if tty {
		old, err := term.MakeRaw(fd)
		if err != nil {
			return newError("tty_error", err.Error())
		}
		defer term.Restore(fd, old)
		restore := enableVT(out)
		defer restore()
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Keystrokes to the guest. The read cannot be interrupted, so it is not waited for.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := a.Stdin.Read(buf)
			if n > 0 {
				if werr := t.WriteFrame(guestagent.TerminalData, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	if tty {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(500 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					w, h, err := term.GetSize(out)
					if err != nil || (w == cols && h == rows) {
						continue
					}
					cols, rows = w, h
					payload := make([]byte, 4)
					binary.BigEndian.PutUint16(payload, uint16(w))
					binary.BigEndian.PutUint16(payload[2:], uint16(h))
					_ = t.WriteFrame(guestagent.TerminalResize, payload)
				}
			}
		}()
	}
	// Closing the terminal on cancel unblocks the frame read.
	go func() {
		select {
		case <-ctx.Done():
			t.Close()
		case <-stop:
		}
	}()
	code, err := pumpTerminal(t, a.Stdout)
	close(stop)
	wg.Wait()
	if err != nil && ctx.Err() == nil {
		return newError("terminal_error", err.Error())
	}
	if code != 0 {
		return &exitError{code: code}
	}
	return nil
}

// pumpTerminal copies guest output to w until the shell exits, returning its exit code.
func pumpTerminal(t *guestagent.Terminal, w io.Writer) (int, error) {
	for {
		kind, payload, err := t.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, nil
			}
			return 0, err
		}
		switch kind {
		case guestagent.TerminalData:
			if _, err := w.Write(payload); err != nil {
				return 0, err
			}
		case guestagent.TerminalExit:
			if len(payload) == 4 {
				return int(int32(binary.BigEndian.Uint32(payload))), nil
			}
			return 0, nil
		}
	}
}

// localPortForward listens on 127.0.0.1:localPort and relays each connection to the
// dock port through the guest agent, until interrupted.
func (a *App) localPortForward(ctx context.Context, e *local.Engine, ref string, port, localPort int, asJSON bool) error {
	// Fail early when the dock is stopped or nothing listens in it.
	probe, err := e.OpenPort(ctx, ref, port)
	if err != nil {
		return localError(err)
	}
	probe.Close()
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
	if err != nil {
		return newError("port_in_use", fmt.Sprintf("cannot listen on 127.0.0.1:%d: %v (use --local-port)", localPort, err))
	}
	defer l.Close()
	actual := l.Addr().(*net.TCPAddr).Port
	url := "http://127.0.0.1:" + strconv.Itoa(actual)
	if asJSON {
		if err := a.printValue(map[string]any{"port": port, "localPort": actual, "url": url}); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(a.Stdout, "%s -> %s:%d (Ctrl+C to stop)\n", url, ref, port)
	}
	go func() { <-ctx.Done(); l.Close() }()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return newError("port_error", err.Error())
		}
		go func() {
			defer conn.Close()
			up, err := e.OpenPort(ctx, ref, port)
			if err != nil {
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(up, conn); done <- struct{}{} }()
			go func() { _, _ = io.Copy(conn, up); done <- struct{}{} }()
			<-done
		}()
	}
}
