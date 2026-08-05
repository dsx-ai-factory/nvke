#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.


# Description:
# This script searches for a network interface that is in the 'down' state,
# starts with 'ens', and is not a Calico interface. If such an interface is found,
# it deletes and re-adds the interface with specified parameters.

# Exit Codes:
# 0 - No errors found
# 1 - Errors found

output=$(lnetctl net show --net tcp)

# Attempt to get an interface that starts with 'ens' and is down
interface=$(echo "$output" | grep -A2 'status: down' | awk '/interfaces:/ {getline} $2 ~ /^ens/ {print $2; exit}')

# If no 'ens' interface is found, attempt to find any non-Calico interface that is down
if [ -z "$interface" ]; then
    echo "No 'ens' interface is down. Attempting to find a non-Calico interface that is down..."
    interface=$(echo "$output" | grep -A2 'status: down' | grep '0:' | grep -v 'calico' | awk -F': ' '{print $2}' | head -n1)
fi

# Check if an interface was found
if [ -z "$interface" ]; then
    echo "All interfaces are up, or no interfaces meet the criteria."
    exit 0
fi

# Display the found interface
echo "Interface found: $interface"

# Delete the identified interface
echo "Deleting interface '$interface'..."
if lnetctl net del --net=tcp --if="$interface"; then
    echo "Interface '$interface' deleted successfully."
else
    echo "Error: Failed to delete interface '$interface'."
    exit 1
fi

# Re-add the interface with specified parameters
echo "Re-adding interface '$interface' with parameters '--peer-credits 32 --credits 512'..."
if lnetctl net add --net=tcp --if="$interface" --peer-credits 32 --credits 512; then
    echo "Interface '$interface' added successfully with the specified parameters."
    exit 0
else
    echo "Error: Failed to add interface '$interface'."
    exit 1
fi
