# Image to Video

The image-video skill animates one supplied image from a written request.

## Inputs

- prompt: the required user instruction.
- image_id: exactly one image identifier.

## Steps

1. Call aurora.seedance_generate with prompt and image_id.

## Required outputs

- One primary video artifact identifier returned by aurora.seedance_generate.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
