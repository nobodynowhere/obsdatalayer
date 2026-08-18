# SBOM Generation

SBOM can be rapidly generated using syft, which is an open source tool for SBOM generation.

## Installation

1 Download syft from https://github.com/anchore/syft/releases
2 Add syft to your path
3 Run syft

## Usage

In the cpdb main directory executee the following command:
```shell
syft scan dir:. --source-name "cpdb" --source-version "26.1.1" --exclude ./.git --exclude ./ui/node_modules -o syft-json=sbom/syft.json -o spdx-json=sbom/spdx.json -o syft-text=sbom/sbom.txt -o syft-table=sbom/table.txt

```

## Quick Vulnerability Scan

You can use grype, a sister tool to syft to generate a quick vulnerability scan. Execute the following command in the cpdb main directory:

```shell
grype sbom/syft.json
```