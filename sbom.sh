#!/usr/bin/env bash

# SBOM Generation and Security Scanning Script
# This script generates SBOM files using Syft and runs vulnerability scanning using Grype

# Extract version from obsgateway.yml
package_name=obsgateway
package_version=$(grep 'version:' "$package_name.yml" | awk '{print $2}' | tr -d '"' | sed 's/^v//')

echo "Generating SBOM and running security scanning..."
echo "Package: $package_name"
echo "Version: $package_version"

# Create sbom directory if it doesn't exist
mkdir -p sbom

# Generate SBOM using Syft
echo "Generating SBOM with Syft..."
syft scan dir:. \
  --source-name "$package_name" \
  --source-version "$package_version" \
  --exclude ./.git \
  --exclude ./ui/node_modules \
  -o syft-json=sbom/syft.json \
  -o spdx-json=sbom/spdx.json \
  -o syft-text=sbom/sbom.txt \
  -o syft-table=sbom/table.txt

if [ $? -ne 0 ]; then
    echo "SBOM generation failed."
    exit 1
fi

echo "SBOM generation completed successfully."

# Run vulnerability scanning using Grype
echo "Running vulnerability scanning with Grype..."
grype sbom/syft.json

if [ $? -ne 0 ]; then
    echo "Security scanning completed with warnings/errors"
else
    echo "Security scanning completed successfully"
fi