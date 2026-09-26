# Resume

The resume skill builds a resume document from a written request and an optional source document.

## Inputs

- prompt: the required user instruction.
- document_id: zero or one document identifier.

## Steps

1. When document_id is present, call aurora.read_document with document_id.
2. Call aurora.render_resume with prompt and the read document text when present.

## Required outputs

- One PDF artifact identifier and one Markdown artifact identifier returned by aurora.render_resume.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
