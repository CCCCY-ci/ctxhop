package main

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const installerWelcomeArgument = "--installer-welcome"

const ctxhopASCIILogo = `        #######       ++++++++++++++++++++++
        #########     ++++++++++++++++++++++++
        ##########                    +++++++++
          ##########                    ++++++++++
            ##########                   ++++++++++
              ##########                   +++++++++           ██████╗ ████████╗ ██╗  ██╗ ██╗  ██╗  ██████╗  ██████╗
               ##########                  +++++++++         ██╔════╝ ╚══██╔══╝ ╚██╗██╔╝ ██║  ██║ ██╔═══██╗ ██╔══██╗
                 #########               +++++++++          ██║         ██║     ╚███╔╝  ███████║ ██║   ██║ ██████╔╝
                ##########             ++++++++++          ██║         ██║     ██╔██╗  ██╔══██║ ██║   ██║ ██╔═══╝
              ###########             +++++++++           ╚██████╗    ██║    ██╔╝ ██╗ ██║  ██║ ╚██████╔╝ ██║
             #########        +++++++  ++++++             ╚═════╝    ╚═╝    ╚═╝  ╚═╝ ╚═╝  ╚═╝  ╚═════╝  ╚═╝
          ###########       ++++++++
         ##########       ++++++++
       ##########       +++++++++   ################
       ########       +++++++++    ##################
        #####         +++++++       ################
`

const installerBannerWidth = 116

func writeInstallerWelcome(w io.Writer) error {
	_, err := fmt.Fprintf(w, `%s
%s

%s

%s
%s

`, ctxhopASCIILogo,
		strings.Repeat("-", installerBannerWidth),
		centerInstallerWelcomeLine("CtxHop "+installerVersionLabel(version)),
		centerInstallerWelcomeLine("Installation complete"),
		centerInstallerWelcomeLine("Run: ctxhop init"),
	)
	return err
}

func installerVersionLabel(value string) string {
	if strings.HasPrefix(strings.ToLower(value), "v") {
		return value
	}
	return "v" + value
}

func centerInstallerWelcomeLine(value string) string {
	padding := (installerBannerWidth - utf8.RuneCountInString(value)) / 2
	if padding <= 0 {
		return value
	}
	return strings.Repeat(" ", padding) + value
}
