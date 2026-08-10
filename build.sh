#!/usr/bin/env bash
commit=$(git rev-parse HEAD)
buildtime=$(date +%Y-%m-%dT%T%z)
package_name=obsgateway

# Parse flags
skip_rpm=false
skip_container=false
for arg in "$@"; do
    case $arg in
        -skiprpm)
            skip_rpm=true
            ;;
        -skipcontainer)
            skip_container=true
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