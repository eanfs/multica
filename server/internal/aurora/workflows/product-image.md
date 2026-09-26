# Product Image

The product-image skill turns a written request and optional product references into product images.

## Inputs

- prompt: the required user instruction.
- image_ids: zero to four product or reference image identifiers, possibly empty.

## Steps

1. When image_ids is empty, call aurora.openai_image with prompt.
2. When image_ids is not empty, call aurora.openai_image with prompt and image_ids.

## Required outputs

- One or more primary image artifact identifiers returned by the tool that ran.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
