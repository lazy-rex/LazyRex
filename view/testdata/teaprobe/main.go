// teaprobe is a probe program for the thread-view facts (docs/rex-protocol.md), not part of the
// renderer. It uses the renderer's Bubble Tea version (view/go.mod). Build: go build ./testdata/teaprobe
//
//	teaprobe tea [-delay 2s] [-mode inline|alt] [-bytes FILE]
//	  Starts in inline mode with "Connecting…", like the renderer. After -delay the first tree
//	  arrives and the program chooses -mode. With -bytes, the program puts the terminal in raw
//	  mode itself and gives Bubble Tea a reader that first appends each read to FILE in hex (one
//	  line per read): the bytes before the parser, beside the key messages after it. The terminal
//	  is then raw for output too (no CR on a newline), so a -bytes run shows the input only: its
//	  drawing is not evidence.
//	  Keys: p prints one line above the live region (tea.Println), m prints 30 (rex send-key P
//	  sends a lowercase p), a switches the alt screen, q quits. The live region shows
//	  "LIVE p=<printed> | <Connecting… or tree> alt=<bool> <cols>x<rows> r=<redraws>" and the
//	  last 10 key messages.
//	teaprobe raw
//	  Puts the terminal in raw mode and prints the bytes of each read in hex. Ctrl+C quits.
//
// A printed line is "L<nn>|" + 100 cells + "|E". At a width under 107 the line is wider than the
// terminal: the terminal wraps it ("|E" on a later row) or cuts it ("|E" is missing). A line that
// the program prints again has its number twice.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
)

type firstTree struct{}

type model struct {
	alt, ready   bool
	mode         string
	w, h, redraw int
	printed      int
	keys         []string
}

func (m *model) Init() tea.Cmd { return nil }

func line(n int) string {
	body := strings.Repeat("abcdefghij", 10)
	return fmt.Sprintf("L%02d|%s|E", n, body)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case firstTree:
		m.ready = true
		m.alt = m.mode == "alt"
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tea.KeyPressMsg:
		k := msg.Key()
		m.keys = append(m.keys, fmt.Sprintf("%q code=%d mod=%d text=%q", msg.String(), k.Code, k.Mod, k.Text))
		if len(m.keys) > 10 {
			m.keys = m.keys[1:]
		}
		switch msg.String() {
		case "q":
			return m, tea.Quit
		case "a":
			m.alt = !m.alt
		case "p":
			m.printed++
			return m, tea.Println(line(m.printed))
		case "m":
			var cmds []tea.Cmd
			for range 30 {
				m.printed++
				cmds = append(cmds, tea.Println(line(m.printed)))
			}
			return m, tea.Sequence(cmds...)
		}
	}
	return m, nil
}

func (m *model) View() tea.View {
	m.redraw++
	state := "Connecting…"
	if m.ready {
		state = "tree"
	}
	// Short enough for 50 columns: Bubble Tea cuts a live row at the terminal width.
	s := fmt.Sprintf("LIVE p=%d | %s alt=%v %dx%d r=%d", m.printed, state, m.alt, m.w, m.h, m.redraw)
	for _, k := range m.keys {
		s += "\n  key " + k
	}
	v := tea.NewView(s)
	v.AltScreen = m.alt
	return v
}

// tee writes each read to log in hex before the parser gets it. A write error ends the input.
type tee struct {
	r   io.Reader
	log *os.File
}

func (t tee) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		// A lost log line would make the record wrong: a write error ends the input.
		if _, werr := fmt.Fprintf(t.log, "%s % x\n", time.Now().Format("15:04:05.000"), p[:n]); werr != nil {
			return 0, werr
		}
	}
	return n, err
}

func raw() error {
	old, err := term.MakeRaw(os.Stdin.Fd())
	if err != nil {
		return err
	}
	defer term.Restore(os.Stdin.Fd(), old)
	fmt.Print("raw: type keys; Ctrl+C quits\r\n")
	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return err
		}
		fmt.Printf("% x\r\n", buf[:n])
		if n == 1 && buf[0] == 3 {
			return nil
		}
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "raw" {
		if err := raw(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	fs := flag.NewFlagSet("tea", flag.ExitOnError)
	delay := fs.Duration("delay", 2*time.Second, "time until the first tree")
	mode := fs.String("mode", "inline", "the mode for the first tree: inline or alt")
	bytesFile := fs.String("bytes", "", "append the input bytes to this file in hex")
	if len(os.Args) > 1 {
		fs.Parse(os.Args[2:])
	}
	var opts []tea.ProgramOption
	if *bytesFile != "" {
		log, err := os.OpenFile(*bytesFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer log.Close()
		old, err := term.MakeRaw(os.Stdin.Fd())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer term.Restore(os.Stdin.Fd(), old)
		opts = append(opts, tea.WithInput(tee{os.Stdin, log}))
	}
	p := tea.NewProgram(&model{mode: *mode}, opts...)
	go func() { time.Sleep(*delay); p.Send(firstTree{}) }()
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
