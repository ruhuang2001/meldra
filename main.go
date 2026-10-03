package main

import "meldra/internal/app"

// version is set by GoReleaser and local builds.
var version = "dev"

func main() { app.Main(version) }
