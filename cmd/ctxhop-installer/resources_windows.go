//go:build windows

package main

// Regenerate the checked-in Windows resources after changing the icon.
//go:generate go run github.com/tc-hib/go-winres@v0.3.3 simply --arch amd64,arm64 --icon ../../assets/ctxhop-logo.png --manifest gui --out rsrc --product-name "CtxHop Installer" --file-description "CtxHop Installer" --original-filename CtxHop-Setup.exe --copyright "Copyright (c) The CtxHop Authors"
