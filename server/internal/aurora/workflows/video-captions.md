# Video Captions

The video-captions skill transcribes one supplied video and renders captions onto it.

This skill runs in a managed sandbox with no shell and no Multica CLI. The only execution path is the reviewed Aurora MCP broker; call its tools by the qualified Claude names given below.

## Inputs

- prompt: the required user instruction.
- attachment_id: exactly one staged video identifier.

## Steps

1. Call the brokered MCP tool `mcp__aurora__aurora_volc_asr_transcribe` (broker method `aurora.volc_asr_transcribe`) with the staged video as `attachment_id`. It returns the transcript `text`.
2. Build caption `cues` from that transcript text, then call `mcp__aurora__aurora_render_video_captions` (broker method `aurora.render_video_captions`) with the same video `attachment_id`, `cues`, and an optional `output_name`.

## Required outputs

- One primary video artifact identifier and one transcript artifact identifier returned by the tools.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
