# Xiaohongshu Copy

The xhs-copy skill writes social copy from a written request.

## Inputs

- prompt: the required user instruction.
- document_id: zero or one document identifier; when present, its extracted text is supplied with the prompt.

## Steps

1. When document_id is present, call aurora.read_document with document_id.
2. Call aurora.write_text_artifact with prompt and the supplied document text when present.

## Required outputs

- One primary text artifact identifier returned by aurora.write_text_artifact.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
