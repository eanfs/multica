# Transcription

The transcription skill turns one supplied audio or video recording into a transcript.

This skill runs in a managed sandbox with no shell and no Multica CLI. The only execution path is the reviewed Aurora MCP broker; call its tools by the qualified Claude names given below.

## Inputs

- prompt: the required user instruction.
- attachment_id: exactly one staged audio or video identifier.

## Steps

1. Call the brokered MCP tool `mcp__aurora__aurora.volc_asr_transcribe` (broker method `aurora.volc_asr_transcribe`) with the staged recording as `attachment_id` and an optional `output_name`.

## Required outputs

- One transcript artifact identifier returned by `aurora.volc_asr_transcribe`.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
