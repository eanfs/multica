# Product Image

The product-image skill turns a written request and optional product references into product images.

This skill runs in a managed sandbox with no shell and no Multica CLI. The only execution path is the reviewed Aurora MCP broker; call its tools by the qualified Claude names given below.

## Inputs

- prompt: the required user instruction.
- attachment_ids: zero to four staged product or reference image identifiers, possibly empty.

## Steps

1. When `attachment_ids` is empty, call the brokered MCP tool `mcp__aurora__aurora_openai_image` (broker method `aurora.openai_image`) with `prompt`.
2. When `attachment_ids` is not empty, call `mcp__aurora__aurora_openai_image` with `prompt` and `attachment_ids`.

## Required outputs

- One or more primary image artifact identifiers returned by `aurora.openai_image`.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
