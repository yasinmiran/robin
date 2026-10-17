`make` writes the gzipped agent builds (`robin-agent-linux-<goarch>.gz`)
here for the CLI to embed. They are not tracked; this file keeps the directory
so the package compiles without them.
