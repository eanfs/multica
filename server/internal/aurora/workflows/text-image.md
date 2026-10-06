# Text to Image

The text-image skill turns a written request into one image.

This skill runs in a managed sandbox with no shell and no Multica CLI. The only execution path is the reviewed Aurora MCP broker; call its tools by the qualified Claude names given below.

## Inputs

- prompt: the required user instruction.

## Steps

1. Call the brokered MCP tool `mcp__aurora__aurora.seedream_generate` (broker method `aurora.seedream_generate`) with `prompt`.

## Required outputs

- One primary image artifact identifier returned by `aurora.seedream_generate`.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
