# ID Photo

The id-photo skill turns one supplied portrait into an identification photo.

This skill runs in a managed sandbox with no shell and no Multica CLI. The only execution path is the reviewed Aurora MCP broker; call its tools by the qualified Claude names given below.

## Inputs

- prompt: the required user instruction.
- attachment_id: exactly one staged image identifier.

## Steps

1. Call the brokered MCP tool `mcp__aurora__aurora_id_photo` (broker method `aurora.id_photo`) with the staged image as `attachment_id` and an optional `output_name`.

## Required outputs

- One primary image artifact identifier returned by `aurora.id_photo`.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
