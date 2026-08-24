//go:build windows

package main

// Regenerate the checked-in Windows resources after changing the icon.
//go:generate go run github.com/tc-hib/go-winres@v0.3.3 simply --arch amd64,arm64 --icon ../../assets/ctxhop-icon.png --manifest cli --out rsrc --product-name CtxHop --file-description "CtxHop command-line interface" --original-filename ctxhop.exe --copyright "Copyright (c) The CtxHop Authors"
