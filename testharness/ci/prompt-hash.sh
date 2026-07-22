#!/usr/bin/env bash
# Fingerprint of the LLM authoring behavior. The committed VCR cassette
# (testharness/ci/cassette.json) is a recording of the model authoring the
# stock watcher under this exact prompt; when the prompt changes the recording
# is stale and must be re-recorded against the real model (publish a release, or
# run the vcr-record workflow, then commit the refreshed cassette).
#
# Kept deliberately narrow: the plugin's authoring instructions in prompt.txt
# are the dominant driver. A model or authoring-message change is NOT captured
# here — bump this if those become part of the fingerprint.
#
# Run from the repo root. Prints the hex sha256, nothing else.
set -euo pipefail

sha256sum internal/plugin/prompt.txt | cut -d' ' -f1
