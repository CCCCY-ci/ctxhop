package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
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

var installerWordmarkGray = [...]uint8{255, 200, 140, 86, 40, 15}

const (
	installerWordmarkStartLine   = 5
	installerWordmarkStartColumn = 57
	ansiReset                    = "\x1b[0m"
)

func writeInstallerWelcome(w io.Writer) error {
	_, err := fmt.Fprintf(w, `%s
%s

%s

%s
%s

`, renderInstallerBanner(installerWelcomeANSIEnabled(w)),
		strings.Repeat("-", installerBannerWidth),
		centerInstallerWelcomeLine("CtxHop "+installerVersionLabel(version)),
		centerInstallerWelcomeLine("Installation complete"),
		centerInstallerWelcomeLine("Run: ctxhop init"),
	)
	return err
}

func renderInstallerBanner(gradient bool) string {
	if !gradient {
		return ctxhopASCIILogo
	}
	lines := strings.Split(ctxhopASCIILogo, "\n")
	for gradientIndex, gray := range installerWordmarkGray {
		lineIndex := installerWordmarkStartLine + gradientIndex
		if lineIndex >= len(lines) || len(lines[lineIndex]) < installerWordmarkStartColumn {
			continue
		}
		color := fmt.Sprintf("\x1b[38;2;%d;%d;%dm", gray, gray, gray)
		line := lines[lineIndex]
		lines[lineIndex] = line[:installerWordmarkStartColumn] + color + line[installerWordmarkStartColumn:] + ansiReset
	}
	return strings.Join(lines, "\n")
}

func installerWelcomeANSIEnabled(w io.Writer) bool {
	file, ok := w.(*os.File)
	return ok && term.IsTerminal(int(file.Fd())) && enableInstallerWelcomeANSI(file)
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
