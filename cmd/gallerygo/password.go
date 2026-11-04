package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

func readPassword(input io.Reader, out io.Writer, fromStdin bool) (string, error) {
	if fromStdin {
		data, err := io.ReadAll(io.LimitReader(input, 75))
		if err != nil {
			return "", err
		}
		defer clear(data)
		data = bytes.TrimSuffix(data, []byte("\n"))
		data = bytes.TrimSuffix(data, []byte("\r"))
		if len(data) > 72 || bytes.ContainsAny(data, "\r\n") {
			return "", errors.New("password input must be a single line of at most 72 UTF-8 bytes")
		}
		return string(data), nil
	}
	file, ok := input.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return "", errors.New("interactive terminal required; use --password-stdin explicitly for automation")
	}
	if _, err := fmt.Fprint(out, "Password (15+ characters, at most 72 UTF-8 bytes): "); err != nil {
		return "", err
	}
	password, err := term.ReadPassword(int(file.Fd()))
	fmt.Fprintln(out)
	if err != nil {
		return "", err
	}
	defer clear(password)
	if _, err := fmt.Fprint(out, "Confirm password: "); err != nil {
		return "", err
	}
	confirm, err := term.ReadPassword(int(file.Fd()))
	fmt.Fprintln(out)
	if err != nil {
		return "", err
	}
	defer clear(confirm)
	if !bytes.Equal(password, confirm) {
		return "", errors.New("passwords do not match")
	}
	return string(password), nil
}
