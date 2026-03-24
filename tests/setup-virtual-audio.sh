#!/bin/bash
# setup-virtual-audio.sh
# Creates a PulseAudio/PipeWire virtual audio sink for Ghost Whisper testing.
# This enables agent-automated audio injection without a physical microphone.

set -e

SINK_NAME="ghost-wispr-test"
SINK_DESC="Ghost-Wispr-Test"

echo "Setting up virtual audio device: $SINK_NAME"

# Check if PulseAudio is running
if ! pgrep -x "pulseaudio" > /dev/null && ! pgrep -x "pipewire" > /dev/null; then
    echo "Error: Neither PulseAudio nor PipeWire is running"
    echo "Please start your audio server first:"
    echo "  systemctl --user start pulseaudio"
    echo "  or"
    echo "  systemctl --user start pipewire"
    exit 1
fi

# Detect which audio server is running
if pgrep -x "pipewire" > /dev/null; then
    echo "Detected PipeWire audio server"
    AUDIO_SERVER="pipewire"
else
    echo "Detected PulseAudio audio server"
    AUDIO_SERVER="pulseaudio"
fi

# Create virtual sink using pactl (works with both PulseAudio and PipeWire)
echo "Creating virtual sink: $SINK_NAME"

# First, check if the sink already exists
if pactl list sinks | grep -q "Name: $SINK_NAME"; then
    echo "Virtual sink '$SINK_NAME' already exists, skipping creation"
else
    # Create null sink (virtual audio device)
    pactl load-module module-null-sink \
        sink_name="$SINK_NAME" \
        sink_properties="device.description='$SINK_DESC'"
    
    if [ $? -eq 0 ]; then
        echo "✓ Virtual sink created successfully"
    else
        echo "Error: Failed to create virtual sink"
        exit 1
    fi
fi

# List available sinks for verification
echo ""
echo "Available audio sinks:"
pactl list sinks | grep -E "^\s+Name:|device.description"

echo ""
echo "Virtual audio device setup complete!"
echo ""
echo "To use this virtual sink with Ghost Whisper:"
echo "1. Set PortAudio to use the virtual source as default input"
echo "2. Inject audio using: paplay --device=$SINK_NAME <audio-file>"
echo ""
echo "Example test:"
echo "  paplay --device=$SINK_NAME tests/fixtures/sample-speech.wav"
echo ""
echo "To remove the virtual sink later, run:"
echo "  pactl unload-module module-null-sink"
