#!/usr/bin/env bash
commit=$(git rev-parse HEAD)
buildtime=$(date +%Y-%m-%dT%T%z)
package_name=obsgateway

# Parse flags
skip_rpm=false
skip_container=false
skip_ui=false
for arg in "$@"; do
    case $arg in
        -skiprpm)
            skip_rpm=true
            ;;
        -skipcontainer)
            skip_container=true
            ;;
        -skipui)
            skip_ui=true
            ;;
    esac
done

# Extract the version from yml
package_version=$(grep 'version:' "$package_name.yml" | awk '{print $2}' | tr -d '"' | sed 's/^v//')
platforms=("linux/amd64")
echo Building::
echo  - Version $package_version
echo  - Commit $commit
echo  - Build Time $buildtime

# Build the admin UI. The bundle is compiled into the binary from
# internal/ui/dist, so it has to be produced before the Go build below.
# Use -skipui to reuse the bundle already in the tree.
if [ "$skip_ui" = false ]; then
    echo "Building UI..."
    # npm ci needs a lock file that matches package.json. No lock is committed
    # yet because @dds/components resolves only from Dell's internal registry;
    # the first npm install on a machine with access writes one, and it should
    # be committed so later builds are reproducible.
    if [ -f ui/package-lock.json ]; then
        (cd ui && npm ci --no-audit --no-fund && npm run build)
    else
        echo "No ui/package-lock.json found; using npm install (commit the lock it generates)."
        (cd ui && npm install --no-audit --no-fund && npm run build)
    fi
    if [ $? -ne 0 ]; then
        echo 'An error has occurred! Aborting the script execution...'
        exit 1
    fi
fi

if [ ! -f internal/ui/dist/index.html ]; then
    echo "Warning: no UI bundle in internal/ui/dist; /ui/ will report 'ui not built'."
fi

for platform in "${platforms[@]}"
do
   platform_split=(${platform//\// })
   GOOS=${platform_split[0]}
   GOARCH=${platform_split[1]}
   output_name=$package_name'-'$GOOS'-'$GOARCH
   if [ $GOOS = "windows" ]; then
       output_name+='.exe'
   fi

   echo "Building $output_name..."
   env CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build -a -trimpath -ldflags="-s -w -X main.version=$package_version -X main.commit=$commit -X main.buildTime=$buildtime" -o build/$output_name .
   if [ $? -ne 0 ]; then
       echo 'An error has occurred! Aborting the script execution...'
       exit 1
   fi
done

# Generate SBOM and run security scanning
echo "Generating SBOM and running security scanning..."
mkdir -p sbom
syft scan dir:. --source-name "$package_name" --source-version "$package_version" --exclude ./.git --exclude ./ui/node_modules -o syft-json=sbom/syft.json -o spdx-json=sbom/spdx.json -o syft-text=sbom/sbom.txt -o syft-table=sbom/table.txt
grype sbom/syft.json
if [ $? -ne 0 ]; then
    echo 'Security scanning completed with warnings/errors'
else
    echo 'Security scanning completed successfully'
fi

# Added RPM Build
if [ "$skip_rpm" = false ]; then
    nfpm package --config $package_name.yml --packager rpm --target build/
fi

# Create Container
if [ "$skip_container" = false ]; then
    sudo podman build --no-cache -t $package_name:latest .
    # Avoid overwriting existing archive
    rm -f build/$package_name-$package_version.tar
    sudo podman save -o build/$package_name-$package_version.tar $package_name:latest
fi
