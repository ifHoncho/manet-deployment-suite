#!/bin/bash

# Welcome Message
echo -e "\nWelcome to MOBTAK"
echo -e "\nMobile Team Awareness Kit\n"

# Display Central Time
echo "Central Time: $(TZ='America/Chicago' date)"

# Display the current UTC date and time
echo "UTC Time: $(TZ='UTC' date)"

# Display the temperatures from the first two thermal zones
echo -e "\nSystem Temperatures:"
if [ -f /sys/class/thermal/thermal_zone0/temp ]; then
    temp1=$(cat /sys/class/thermal/thermal_zone0/temp)
    temp1=$(echo "scale=2; $temp1 / 1000" | bc)
    echo "Zone 1: $temp1°C"
fi

if [ -f /sys/class/thermal/thermal_zone1/temp ]; then
    temp2=$(cat /sys/class/thermal/thermal_zone1/temp)
    temp2=$(echo "scale=2; $temp2 / 1000" | bc)
    echo "Zone 2: $temp2°C"
fi

# Function to calculate CPU usage
calculate_cpu_usage() {
    local cpu_line1=$(grep 'cpu ' /proc/stat)
    local idle1=$(echo $cpu_line1 | awk '{print $5}')
    local total1=$(echo $cpu_line1 | awk '{print $2+$3+$4+$5+$6+$7+$8}')

    sleep 0.2

    local cpu_line2=$(grep 'cpu ' /proc/stat)
    local idle2=$(echo $cpu_line2 | awk '{print $5}')
    local total2=$(echo $cpu_line2 | awk '{print $2+$3+$4+$5+$6+$7+$8}')

    local idle=$((idle2 - idle1))
    local total=$((total2 - total1))
    local usage=$((100 * (total - idle) / total))

    echo $usage
}

# Get CPU usage
cpu_usage=$(calculate_cpu_usage)

echo -e "CPU Usage: $cpu_usage%\n"