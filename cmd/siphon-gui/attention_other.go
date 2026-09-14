//go:build !windows

package main

import "fyne.io/fyne/v2"

func requestAttention(w fyne.Window) { w.RequestFocus() }

func registerToastIdentity(string) {}
