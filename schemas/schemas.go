// Package schemas holds the JSON schemas for the manifest files - the state files and changesets. They are published
// at https://raw.githubusercontent.com/johnmuhlestein/skima/main/schemas/ so editors can validate a file through its
// "$schema" property, and embedded here so skima manifest validate needs no network
package schemas

import "embed"

// BaseURL is the schemas' $id base
const BaseURL = "https://raw.githubusercontent.com/johnmuhlestein/skima/main/schemas/"

// Files are the schemas, named <kind>.schema.json
//
//go:embed *.schema.json
var Files embed.FS
