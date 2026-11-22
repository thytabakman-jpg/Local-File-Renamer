package main

import "errors"

func nativeFolderPicker(string) (string, error) {
	return "", errors.New("The native Windows folder picker is available on Windows")
}
