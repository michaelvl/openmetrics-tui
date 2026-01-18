# VHS documentation: https://github.com/charmbracelet/vhs
#
# This tape file generates a 20-second GIF showing openmetrics-tui
# displaying live metrics without any user interaction.

# Output settings
Output docs/screenshot.gif

# Set up the terminal appearance
Set FontSize 14
Set Width 1400
Set Height 300
Set Theme "TokyoNight"
Set Padding 10
Set BorderRadius 8
Set Margin 10
Set MarginFill "#1a1b26"

# Set shell
Set Shell bash

# Hide the setup commands
Hide

# Build the binaries if needed
Type "make all 2>/dev/null || true"
Enter
Sleep 1s

# Start the mock server in the background
Type "./mock-server -port 8080 > /dev/null 2>&1 &"
Enter
Sleep 2s

# Clear the screen for a clean start
Type "clear"
Enter

# Show the recording
Show

# Launch openmetrics-tui with a fast update interval for dynamic display
Type "./openmetrics-tui -url http://localhost:8080/metrics -interval 1s -delta-mode next"
Enter

# Wait for initial connection and data to load
Sleep 2s

# Let the metrics update and scroll for 20 seconds
# The mock server generates changing metrics, so this will show live updates
Sleep 20s

# Clean exit
Hide
Type "q"
Sleep 500ms

# Cleanup: kill the mock server
Type "pkill -f mock-server"
Enter
