# Document Summary

The document-summary skill summarises one supplied document.

This skill runs in a managed sandbox with no shell and no Multica CLI. The only execution path is the reviewed Aurora MCP broker; call its tools by the qualified Claude names given below.

## Inputs

- prompt: the required user instruction.
- attachment_id: exactly one staged document identifier.

## Steps

1. Call the brokered MCP tool `mcp__aurora__aurora.read_document` (broker method `aurora.read_document`) with the staged document as `attachment_id` and read the returned text.
2. Write the summary yourself, then call `mcp__aurora__aurora.write_text_artifact` (broker method `aurora.write_text_artifact`) with the summary as `content` and a `.md` or `.txt` `name`.

## Required outputs

- One primary text artifact identifier returned by `aurora.write_text_artifact`.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
