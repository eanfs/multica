# Text to Video

The text-video skill turns a written request into one video.

## Inputs

- prompt: the required user instruction.

## Steps

1. Call aurora.seedance_generate with prompt.

## Required outputs

- One primary video artifact identifier returned by aurora.seedance_generate.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
