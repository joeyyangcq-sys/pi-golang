// Package terminal contains the small amount of terminal-specific behavior
// needed by the interactive CLI. It intentionally has no dependency on the
// agent or usecase layers.
package terminal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInterrupt means that the user cancelled the current input line with
// Ctrl-C. It is not a process-level failure.
var ErrInterrupt = errors.New("terminal input interrupted")

// LineEditor is a deliberately small readline-style editor. It supports
// cursor movement, history, deletion, UTF-8 input, and clean redraws without
// bringing a terminal framework into the minimal CLI.
type LineEditor struct {
	in      *os.File
	out     io.Writer
	prompt  string
	history []string
	state   string
}

// NewLineEditor creates an editor for a character device. The terminal is
// switched to raw input only while ReadLine is active and is restored before
// ReadLine returns, including on errors.
func NewLineEditor(in *os.File, out io.Writer, prompt string) (*LineEditor, error) {
	if in == nil || out == nil {
		return nil, errors.New("line editor: input and output are required")
	}
	if runtime.GOOS == "windows" {
		return nil, errors.New("line editor: raw mode is unavailable on windows")
	}
	return &LineEditor{in: in, out: out, prompt: prompt}, nil
}

// ReadLine reads one edited line without the trailing newline.
func (e *LineEditor) ReadLine() (line string, err error) {
	if err = e.enableRaw(); err != nil {
		return "", err
	}
	defer func() {
		restoreErr := e.disableRaw()
		if err == nil && restoreErr != nil {
			err = restoreErr
		}
	}()

	buffer := make([]rune, 0, 80)
	cursor := 0
	historyIndex := len(e.history)
	savedCurrent := ""
	if _, err = fmt.Fprint(e.out, e.prompt); err != nil {
		return "", err
	}
	redraw := func() error { return e.redraw(buffer, cursor) }

	for {
		b, readErr := e.readByte()
		if readErr != nil {
			return "", readErr
		}
		switch b {
		case '\r', '\n':
			if _, err = fmt.Fprint(e.out, "\r\n"); err != nil {
				return "", err
			}
			line = string(buffer)
			if strings.TrimSpace(line) != "" {
				e.appendHistory(line)
			}
			return line, nil
		case 0x03: // Ctrl-C
			_, _ = fmt.Fprint(e.out, "^C\r\n")
			return "", ErrInterrupt
		case 0x04: // Ctrl-D
			if len(buffer) == 0 {
				_, _ = fmt.Fprint(e.out, "\r\n")
				return "", io.EOF
			}
			if cursor < len(buffer) {
				buffer = append(buffer[:cursor], buffer[cursor+1:]...)
			}
		case 0x01: // Ctrl-A
			cursor = 0
		case 0x05: // Ctrl-E
			cursor = len(buffer)
		case 0x0b: // Ctrl-K
			buffer = buffer[:cursor]
		case 0x15: // Ctrl-U
			buffer = buffer[:0]
			cursor = 0
		case 0x17: // Ctrl-W
			wordStart := cursor
			for cursor > 0 && unicode.IsSpace(buffer[cursor-1]) {
				cursor--
			}
			for cursor > 0 && !unicode.IsSpace(buffer[cursor-1]) {
				cursor--
			}
			buffer = append(buffer[:cursor], buffer[wordStart:]...)
		case 0x7f, 0x08: // Backspace / Ctrl-H
			if cursor > 0 {
				cursor--
				buffer = append(buffer[:cursor], buffer[cursor+1:]...)
			}
		case 0x1b: // Escape sequence, normally an arrow or editing key.
			action, sequenceErr := e.readEscapeSequence()
			if sequenceErr != nil {
				return "", sequenceErr
			}
			switch action {
			case "left":
				if cursor > 0 {
					cursor--
				}
			case "right":
				if cursor < len(buffer) {
					cursor++
				}
			case "home":
				cursor = 0
			case "end":
				cursor = len(buffer)
			case "delete":
				if cursor < len(buffer) {
					buffer = append(buffer[:cursor], buffer[cursor+1:]...)
				}
			case "up":
				if len(e.history) > 0 && historyIndex > 0 {
					if historyIndex == len(e.history) {
						savedCurrent = string(buffer)
					}
					historyIndex--
					buffer = []rune(e.history[historyIndex])
					cursor = len(buffer)
				}
			case "down":
				if historyIndex < len(e.history) {
					historyIndex++
					if historyIndex == len(e.history) {
						buffer = []rune(savedCurrent)
					} else {
						buffer = []rune(e.history[historyIndex])
					}
					cursor = len(buffer)
				}
			}
		default:
			if b < 0x20 {
				continue
			}
			runeValue, runeErr := e.readRune(b)
			if runeErr != nil {
				return "", runeErr
			}
			buffer = append(buffer, 0)
			copy(buffer[cursor+1:], buffer[cursor:])
			buffer[cursor] = runeValue
			cursor++
		}
		if err = redraw(); err != nil {
			return "", err
		}
	}
}

func (e *LineEditor) enableRaw() error {
	command := exec.Command("stty", "-g")
	command.Stdin = e.in
	state, err := command.Output()
	if err != nil {
		return fmt.Errorf("line editor: read terminal state: %w", err)
	}
	e.state = strings.TrimSpace(string(state))
	command = exec.Command("stty", "-icanon", "-echo", "min", "1", "time", "0")
	command.Stdin = e.in
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("line editor: enable raw mode: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (e *LineEditor) disableRaw() error {
	if e.state == "" {
		return nil
	}
	command := exec.Command("stty", e.state)
	command.Stdin = e.in
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("line editor: restore terminal: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	e.state = ""
	return nil
}

func (e *LineEditor) readByte() (byte, error) {
	var one [1]byte
	_, err := e.in.Read(one[:])
	return one[0], err
}

func (e *LineEditor) readRune(first byte) (rune, error) {
	width := utf8.RuneLen(rune(first))
	if width <= 1 {
		return rune(first), nil
	}
	encoded := make([]byte, width)
	encoded[0] = first
	if _, err := io.ReadFull(e.in, encoded[1:]); err != nil {
		return utf8.RuneError, err
	}
	r, size := utf8.DecodeRune(encoded)
	if size == 1 && r == utf8.RuneError {
		return utf8.RuneError, nil
	}
	return r, nil
}

func (e *LineEditor) readEscapeSequence() (string, error) {
	first, err := e.readByte()
	if err != nil {
		return "", err
	}
	if first != '[' && first != 'O' {
		return "", nil
	}
	second, err := e.readByte()
	if err != nil {
		return "", err
	}
	if first == 'O' {
		return escapeAction(string(second)), nil
	}
	if second >= '0' && second <= '9' {
		third, readErr := e.readByte()
		if readErr != nil {
			return "", readErr
		}
		if third == '~' && second == '3' {
			return "delete", nil
		}
		return "", nil
	}
	return escapeAction(string(second)), nil
}

func escapeAction(code string) string {
	switch code {
	case "A":
		return "up"
	case "B":
		return "down"
	case "C":
		return "right"
	case "D":
		return "left"
	case "H":
		return "home"
	case "F":
		return "end"
	default:
		return ""
	}
}

func (e *LineEditor) redraw(buffer []rune, cursor int) error {
	if _, err := fmt.Fprintf(e.out, "\r%s%s\x1b[K", e.prompt, string(buffer)); err != nil {
		return err
	}
	remaining := displayWidth(buffer[cursor:])
	if remaining > 0 {
		_, err := fmt.Fprintf(e.out, "\x1b[%dD", remaining)
		return err
	}
	return nil
}

func (e *LineEditor) appendHistory(line string) {
	if len(e.history) > 0 && e.history[len(e.history)-1] == line {
		return
	}
	e.history = append(e.history, line)
	if len(e.history) > 100 {
		e.history = e.history[len(e.history)-100:]
	}
}

func displayWidth(runes []rune) int {
	width := 0
	for _, r := range runes {
		if r == '\t' {
			width += 4
		} else if isWideRune(r) {
			width += 2
		} else if !unicode.IsControl(r) {
			width++
		}
	}
	return width
}

func isWideRune(r rune) bool {
	return (r >= 0x1100 && r <= 0x115f) ||
		(r >= 0x2e80 && r <= 0xa4cf) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0x1f300 && r <= 0x1faff)
}
