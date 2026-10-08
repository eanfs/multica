# Image Edit

The image-edit skill applies a written instruction to one to four supplied images.

This skill runs in a managed sandbox with no shell and no Multica CLI. The only execution path is the reviewed Aurora MCP broker; call its tools by the qualified Claude names given below.

## Inputs

- prompt: the required user instruction.
- attachment_ids: one to four staged image identifiers.

## Steps

1. Call the brokered MCP tool `mcp__aurora__aurora_seedream_generate` (broker method `aurora.seedream_generate`) with `prompt` and the images in `attachment_ids` as reference images.

## Required outputs

- One or more primary image artifact identifiers returned by `aurora.seedream_generate`.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
