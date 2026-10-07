# Resume

The resume skill builds a resume document from a written request and an optional source document.

This skill runs in a managed sandbox with no shell and no Multica CLI. The only execution path is the reviewed Aurora MCP broker; call its tools by the qualified Claude names given below.

## Inputs

- prompt: the required user instruction.
- attachment_id: zero or one staged document identifier.

## Steps

1. When a document is supplied, call the brokered MCP tool `mcp__aurora__aurora_read_document` (broker method `aurora.read_document`) with the staged document as `attachment_id` and read the returned text.
2. Build the resume `sections` object yourself (name, title, summary, contact, skills, experience, education), then call `mcp__aurora__aurora_render_resume` (broker method `aurora.render_resume`) with `sections` and an optional `output_name`.

## Required outputs

- One PDF artifact identifier and one Markdown artifact identifier returned by `aurora.render_resume`.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
