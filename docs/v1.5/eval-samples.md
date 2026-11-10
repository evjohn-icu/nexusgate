# Permission-cleared evaluation samples

The V1.5 accuracy gate needs licensed speech/sound clips. This note records the
sample sources checked on 2026-11-11, their licences, and what is still needed
before a metric can be reported.

## Verified source: podscripter audio fixtures

- Repository: <https://github.com/algernon725/podscripter>
  (`tests/fixtures/audio/`; code MIT, fixture metadata in Git).
- Audio host: <https://huggingface.co/datasets/podscripter-project/test-fixtures>,
  pinned revision `6e1c1eced6e68bb35b1f1d89c56478229201a2f7`.
- Aggregate licence: **CC-BY-4.0** (components CC0-1.0 and CC-BY-4.0). The
  repository's `LICENSES.md` is the source-of-truth allowlist; it excludes
  CC-BY-NC/ND sources.
- Contents: 23 WAV files, ~7 MB total, 16 kHz mono; English, Spanish and
  French; sourced from AMI, LibriSpeech, Common Voice, FLEURS, MLS and
  VoxPopuli. Each clip has a `.expected.json` with transcript, speaker turns,
  expected speaker count, source, licence, attribution and modifications.
- Useful for: ASR (WER), diarization (DER), speaker trials (same/different),
  pipeline latency. Not useful for visual retrieval or environmental-sound F1.

Two clips were downloaded and verified locally:

| clip | duration | SHA-256 | ground truth |
| --- | --- | --- | --- |
| `en/ami_en2001b_1993_2020.wav` | 25.85 s | `ed3119d1f1e44b77903f1d1ccff7973dc8183171643a59b98dcb03be66058ca3` | 2 speakers, 5 turns, AMI transcript |
| `en/librispeech_test_clean_1089_134686_0000.wav` | 10.435 s | `0b1785dba56f22af426ccb25d318f7e103558fd40e1c3ab064b455dba2afae12` | 1 speaker, LibriSpeech transcript |

Fetch without cloning the audio into Git:

```bash
REV=6e1c1eced6e68bb35b1f1d89c56478229201a2f7
BASE=https://huggingface.co/datasets/podscripter-project/test-fixtures/resolve/$REV
curl -L -o ami.wav "$BASE/en/ami_en2001b_1993_2020.wav"
curl -L -o librispeech.wav "$BASE/en/librispeech_test_clean_1089_134686_0000.wav"
sha256sum ami.wav librispeech.wav
```

The matching annotations are in the GitHub repository under
`tests/fixtures/audio/<language>/<name>.expected.json`.

## Sources checked and rejected for redistribution

- **ESC-50** (<https://github.com/karolpiczak/ESC-50>): 2,000 environmental
  clips, but the dataset is **CC BY-NC 3.0**; usable for internal research only,
  not a redistributable evaluation asset.
- **fastaudio/Audio-Competition** (<https://github.com/fastaudio/Audio-Competition>):
  MIT code, but the repository contains notebooks/scripts, no audio files.
- **ClipShots / Shot2Story**: annotation code only; the videos are YouTube
  links with their own terms, so they cannot be bundled or reliably fetched.
- **OpenCV extra** (`testdata/highgui/video/big_buck_bunny.mp4`, 315 KB):
  a real video clip usable for pipeline/visual smoke tests, but it has no
  retrieval ground truth and the test-data licence must be checked per file.

## Measured speaker slice

CAM++ and ERes2NetV2 were installed from their pinned ModelScope revisions and
verified against the lock hashes. The FunASR adapter now loads the local
`configuration.json` + checkpoint directly (the ModelScope configuration is not
a FunASR `config.yaml`, so passing the directory alone failed).

A two-speaker, 12-utterance set (speechbrain `tests/samples/ASR`, Apache-2.0)
was padded to 3.5 s, embedded through the real `inference.dispatch`
`speaker_embedding` boundary, and scored with
`nexusgate-eval multimodal score`. Calibration = snt1-3, test = snt4-6; 6
same-speaker and 9 different-speaker trials per partition.

- selected threshold: 0.7064 (95% precision target)
- test precision 1.000, recall 0.333, FAR 0.000, FRR 0.667, EER 0.000
- 6 positive / 9 negative test trials

This is an **infrastructure-valid tiny measurement**, not the plan's speaker
gate: two speakers cannot establish a production threshold. CAM++ CPU measured
49.5 ms P50 / 49.7 ms P95 at batch 1; ERes2NetV2 measured 172.9 / 181.2 ms.

## Remaining gate

Samples are no longer the blocker for ASR/speaker work. The metrics that still
cannot be produced on this host are:

- **Diarization DER**: `pyannote/speaker-diarization-community-1` is gated
  (HTTP 401 without accepting terms).
- **Environmental event F1**: YAMNet weights are not installed and the Kaggle
  artifact requires credentials.
- **Speaker EER**: CAM++ weights are catalogued but not installed; the sample
  set also has too few speakers for a calibrated threshold (report
  `not_evaluable` rather than a number).
- **Visual retrieval**: these clips are audio-only; a video set with interval
  ground truth is still required.

No audio from these sources is committed to this repository. Any future
evaluation must record the asset SHA-256, licence and partition in the
`multimodal-eval.v1` manifest.
