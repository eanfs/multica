# Video Captions

The video-captions skill transcribes one supplied video and renders captions onto it.

## Inputs

- prompt: the required user instruction.
- video_id: exactly one video identifier.

## Steps

1. Call aurora.volc_asr_transcribe with video_id.
2. Call aurora.render_video_captions with the returned transcript artifact identifier and video_id.

## Required outputs

- One primary video artifact identifier and one transcript artifact identifier returned by the tools.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
