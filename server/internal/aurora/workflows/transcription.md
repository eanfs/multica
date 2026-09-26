# Transcription

The transcription skill turns one supplied audio or video recording into a transcript.

## Inputs

- prompt: the required user instruction.
- audio_id or video_id: exactly one recording identifier.

## Steps

1. Call aurora.volc_asr_transcribe with the supplied input identifier.

## Required outputs

- One transcript artifact identifier returned by aurora.volc_asr_transcribe.

## Failure behavior

- Stop immediately when any tool returns an error.
- Never retry a tool that creates an artifact.
- Never claim completion unless the tool returned every required artifact identifier.
