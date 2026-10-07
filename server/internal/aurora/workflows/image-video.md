# Image to Video

The image-video skill animates one supplied image from a written request.

This skill runs in a managed sandbox with no shell and no Multica CLI. The only execution path is the reviewed Aurora MCP broker; call its tools by the qualified Claude names given below.

## Inputs

- prompt: the required user instruction.
- attachment_ids: exactly one staged image identifier.

## Steps

1. Call the brokered MCP tool `mcp__aurora__aurora_seedance_generate` (broker method `aurora.seedance_generate`) with `prompt` and the staged image in `attachment_ids`.

## Required outputs

- One primary video artifact identifier returned by `aurora.seedance_generate`.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
