# Document Summary

The document-summary skill summarises one supplied document.

## Inputs

- prompt: the required user instruction.
- document_id: exactly one document identifier.

## Steps

1. Call aurora.read_document with document_id.
2. Call aurora.render_text_artifact with prompt and the returned document text.

## Required outputs

- One primary text artifact identifier returned by aurora.render_text_artifact.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
