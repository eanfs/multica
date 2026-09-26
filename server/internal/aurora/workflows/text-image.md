# Text to Image

The text-image skill turns a written request into one image.

## Inputs

- prompt: the required user instruction.

## Steps

1. Call aurora.seedream_generate with prompt.

## Required outputs

- One primary image artifact identifier returned by aurora.seedream_generate.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
