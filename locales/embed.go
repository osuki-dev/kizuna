package locales

import "embed"

// Content embeds all locale JSON definitions from the root locales directory
//
//go:embed *.json
var Content embed.FS
