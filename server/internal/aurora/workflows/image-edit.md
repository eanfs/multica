# Image Edit

The image-edit skill applies a written instruction to one to four supplied images.

## Inputs

- prompt: the required user instruction.
- image_ids: one to four image identifiers.

## Steps

1. Call aurora.openai_edit_image with prompt and image_ids.

## Required outputs

- One or more primary image artifact identifiers returned by aurora.openai_edit_image.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
