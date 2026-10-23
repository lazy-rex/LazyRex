// teaprobe is a probe program for the thread-view facts (docs/rex-protocol.md), not part of the
// renderer. It uses the renderer's Bubble Tea version (view/go.mod). Build: go build ./testdata/teaprobe
//
//	teaprobe tea [-delay 2s] [-mode inline|alt]
//	  Starts in inline mode with "Connecting…", like the renderer. After -delay the first tree
//	  arrives and the program chooses -mode. Keys: p prints one line above the live region
//	  (tea.Println), P prints 30, a switches the alt screen, q quits. The live region shows the
//	  size, the mode, the redraw count and the last 10 key messages.
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
		case "P":
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
	s := fmt.Sprintf("LIVE %s alt=%v size=%dx%d redraw=%d printed=%d", state, m.alt, m.w, m.h, m.redraw, m.printed)
	for _, k := range m.keys {
		s += "\n  key " + k
	}
	v := tea.NewView(s)
	v.AltScreen = m.alt
	return v
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
	if len(os.Args) > 1 {
		fs.Parse(os.Args[2:])
	}
	p := tea.NewProgram(&model{mode: *mode})
	go func() { time.Sleep(*delay); p.Send(firstTree{}) }()
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
