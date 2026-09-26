# ID Photo

The id-photo skill turns one supplied portrait into an identification photo.

## Inputs

- prompt: the required user instruction.
- image_id: exactly one image identifier.

## Steps

1. Call aurora.id_photo with prompt and image_id.

## Required outputs

- One primary image artifact identifier returned by aurora.id_photo.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
