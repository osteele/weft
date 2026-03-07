package agentdeploy

import "embed"

//go:embed all:binaries/*
var agentBinaries embed.FS
