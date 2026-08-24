package main

import (
	"fmt"
	"io"
)

const installerWelcomeArgument = "--installer-welcome"

const ctxhopASCIILogo = `
       \                    __________
        \                  /          \
         >                /            >
        /                  \          /
       /                    \________/
                              /    __________
                             /    __________
`

func writeInstallerWelcome(w io.Writer) error {
	_, err := fmt.Fprintf(w, `%s
CtxHop %s

Installation complete.

Get started:
  ctxhop init

`, ctxhopASCIILogo, version)
	return err
}
