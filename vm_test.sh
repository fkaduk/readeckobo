#!/usr/bin/env bash
set -Eeuo pipefail

# Host-side orchestration for the end-to-end test, which
# - starts Readeck docker container
# - assembles a tiny guest VM containing the release tarball
# - boots it under 32-bit ARM emulation
# - passes control over to vm_test_guest.sh

readonly alpine_base_url=https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/armv7/netboot-3.24.1
readonly alpine_repository_url=http://dl-cdn.alpinelinux.org/alpine/v3.24/main
readonly artifact_cache_dir=.cache/kobodeck-testvm
readonly readeck_image=codeberg.org/readeck/readeck:latest
readonly admin_user=testadmin
readonly admin_pass=testpass123
readonly admin_email=testadmin@test.invalid
readonly article_url=https://example.com
readonly qemu_host_gateway=10.0.2.2
readonly coverage_enabled=${KOBODECK_COVERAGE:-}
readonly coverage_output_dir=build/e2e-coverdata
readonly coverage_profile=build/e2e-coverage.out
if [[ -n "$coverage_enabled" ]]; then
	readonly tarball=build/KoboRoot.cover.tgz
else
	readonly tarball=build/KoboRoot.tgz
fi
readeck_container_id=
temporary_work_dir=
onboard_disk_image=

cleanup() {
	if [[ -n "$readeck_container_id" ]]; then
		docker stop --time 1 "$readeck_container_id" >/dev/null 2>&1 || true
		readeck_container_id=
	fi
	if [[ -n "$temporary_work_dir" ]]; then
		rm -rf "$temporary_work_dir"
		temporary_work_dir=
	fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

die() {
	echo "vm test: $*" >&2
	exit 1
}

check_dependencies() {
	for command in cpio curl docker find gzip mkfs.vfat qemu-system-arm sha256sum timeout truncate; do
		command -v "$command" >/dev/null || die "required command not found: $command"
	done
	[[ -s "$tarball" ]] || die "$tarball is missing; build the requested tarball first"
	if [[ -n "$coverage_enabled" ]]; then
		for command in go mcopy; do
			command -v "$command" >/dev/null || die "coverage mode requires command: $command"
		done
	fi
}

download_artifact() {
	local name=$1
	local want_hash=$2
	local path=$artifact_cache_dir/$name
	local got_hash=

	# confirm cache file via hash
	if [[ -f "$path" ]]; then
		got_hash=$(sha256sum "$path")
		got_hash=${got_hash%% *}
		if [[ "$got_hash" == "$want_hash" ]]; then
			printf '%s\n' "$path"
			return
		fi
	fi

	echo "vm test: downloading $name" >&2
	curl --fail --location --silent --show-error \
		--output "$temporary_work_dir/$name" "$alpine_base_url/$name"
	got_hash=$(sha256sum "$temporary_work_dir/$name")
	got_hash=${got_hash%% *}
	[[ "$got_hash" == "$want_hash" ]] ||
		die "$name sha256 is $got_hash, want $want_hash"
	mv "$temporary_work_dir/$name" "$path"
	printf '%s\n' "$path"
}

download_artifacts() {
	mkdir -p "$artifact_cache_dir"
	kernel=$(download_artifact \
		vmlinuz-lts \
		f36e6732b8165eb43780b86a87a3eb2e17fbabc9984bab9796a9a72c4d56debc)
	base_initramfs=$(download_artifact \
		initramfs-lts \
		32807f8009a39c86bc70d601e9175293d416a7d204275f807e490cd92653cd68)
}

start_readeck() {
	echo "vm test: starting Readeck"
	readeck_container_id=$(docker run --pull always --detach --rm \
		--publish 127.0.0.1::8000 \
		"$readeck_image")

	echo "vm test: waiting for Readeck to become healthy"
	local healthy=false
	for _ in {1..240}; do
		if docker exec "$readeck_container_id" readeck healthcheck >/dev/null 2>&1; then
			healthy=true
			break
		fi
		sleep 0.25
	done
	if [[ "$healthy" != true ]]; then
		docker logs "$readeck_container_id" >&2 || true
		die "Readeck did not become healthy"
	fi

	echo "vm test: Readeck is ready; creating test user"
	docker exec "$readeck_container_id" readeck user \
		-user "$admin_user" \
		-password "$admin_pass" \
		-email "$admin_email" >/dev/null
}

prepare_readeck_test_data() {
	local port_mapping host_port cookie_jar token_page token_pattern
	local bookmark_headers bookmark loaded loaded_pattern

	port_mapping=$(docker port "$readeck_container_id" 8000/tcp)
	host_port=${port_mapping##*:}
	[[ "$host_port" =~ ^[0-9]+$ ]] || die "cannot parse Readeck port mapping: $port_mapping"
	host_url=http://127.0.0.1:$host_port
	vm_url=http://$qemu_host_gateway:$host_port

	cookie_jar=$temporary_work_dir/readeck-cookies
	curl --fail --location --silent --show-error \
		--cookie-jar "$cookie_jar" \
		--data-urlencode "username=$admin_user" \
		--data-urlencode "password=$admin_pass" \
		--data-urlencode 'redirect=' \
		"$host_url/login" >/dev/null
	token_page=$(curl --fail --location --silent --show-error \
		--cookie "$cookie_jar" --data '' "$host_url/profile/tokens")
	token_pattern='value="Authorization: Bearer ([^"]+)"'
	if [[ $token_page =~ $token_pattern ]]; then
		token=${BASH_REMATCH[1]}
	else
		die "API token not found in Readeck profile page"
	fi
	echo "vm test: creating test bookmark in Readeck"
	bookmark_headers=$temporary_work_dir/bookmark-headers
	curl --fail --silent --show-error \
		--dump-header "$bookmark_headers" --output /dev/null \
		--header "Authorization: Bearer $token" \
		--header 'Content-Type: application/json' \
		--data "{\"url\":\"$article_url\"}" \
		"$host_url/api/bookmarks"
	bookmark_id=$(awk 'tolower($1) == "bookmark-id:" {gsub("\r", "", $2); print $2}' \
		"$bookmark_headers")
	[[ -n "$bookmark_id" ]] || die "Readeck response has no Bookmark-Id header"

	echo "vm test: waiting for Readeck to process test bookmark"
	loaded=false
	loaded_pattern='"loaded"[[:space:]]*:[[:space:]]*true'
	for _ in {1..120}; do
		bookmark=$(curl --fail --silent --show-error \
			--header "Authorization: Bearer $token" \
			"$host_url/api/bookmarks/$bookmark_id")
		if [[ $bookmark =~ $loaded_pattern ]]; then
			loaded=true
			break
		fi
		sleep 0.25
	done
	[[ "$loaded" == true ]] || die "Readeck bookmark did not load"
	echo "vm test: Readeck test bookmark is ready"
}

build_vm_filesystem() {
	local overlay overlay_archive

	overlay=$temporary_work_dir/overlay
	mkdir -p "$overlay/etc/apk" "$overlay/test"
	cp vm_test_guest.sh "$overlay/vm_test_guest.sh"
	cp "$tarball" "$overlay/test/KoboRoot.tgz"
	cp testdata/nickel-schema-176.sql "$overlay/test/nickel-schema.sql"
	printf '%s\n' "$bookmark_id" >"$overlay/test/bookmark-id"
	printf '%s\n' "$alpine_repository_url" >"$overlay/etc/apk/repositories"
	echo "vm test: writing Kobodeck test configuration"
	cat >"$overlay/test/kobodeck.toml" <<EOF
[Server]
URL = "$vm_url"
Token = "$token"
Timeout = 30

[Fetch]
Workers = 1
Limit = 0
Labels = ""
Status = "unread,reading"

[Sync]
Archive = true
FavouriteCollection = "VM Favourites"

[Log]
Verbose = false
Size = 1

[Output]
Path = "/mnt/onboard/kobodeck"
Delete = false
EOF
	if [[ -n "$coverage_enabled" ]]; then
		cat >"$overlay/test/90-kobodeck-cover.rules" <<'EOF'
KERNEL=="eth*", ACTION=="add", RUN+="/bin/sh -c 'GOCOVERDIR=/mnt/onboard/kobodeck-coverdata /usr/local/bin/kobodeck &'"
KERNEL=="wlan*", ACTION=="add", RUN+="/bin/sh -c 'GOCOVERDIR=/mnt/onboard/kobodeck-coverdata /usr/local/bin/kobodeck &'"
EOF
	fi
	chmod 755 "$overlay/vm_test_guest.sh"

	overlay_archive=$temporary_work_dir/initramfs-overlay.gz
	(
		cd "$overlay"
		find . -mindepth 1 -print0 | cpio --null --create --format=newc --quiet | gzip -c
	) >"$overlay_archive"
	combined_initramfs=$temporary_work_dir/initramfs-test
	cp "$base_initramfs" "$combined_initramfs"
	cat "$overlay_archive" >>"$combined_initramfs"
}

run_vm() {
	local transcript

	onboard_disk_image=$temporary_work_dir/onboard.fat
	truncate --size 128M "$onboard_disk_image"
	mkfs.vfat "$onboard_disk_image" >/dev/null
	transcript=$temporary_work_dir/qemu.log
	echo "vm test: booting ARM release smoke test"
	if ! timeout 2m qemu-system-arm \
		-machine virt \
		-cpu cortex-a15 \
		-smp 1 \
		-m 256 \
		-display none \
		-monitor none \
		-serial stdio \
		-no-reboot \
		-kernel "$kernel" \
		-initrd "$combined_initramfs" \
		-append 'console=ttyAMA0 rdinit=/vm_test_guest.sh' \
		-netdev user,id=net0 \
		-device virtio-net-device,netdev=net0 \
		-drive "file=$onboard_disk_image,format=raw,if=none,id=onboard" \
		-device virtio-blk-device,drive=onboard \
		>"$transcript" 2>&1; then
		cat "$transcript" >&2
		die "QEMU failed"
	fi
	if ! grep -Fq KOBODECK_VM_PASS "$transcript"; then
		cat "$transcript" >&2
		die "guest did not report success"
	fi
}

collect_coverage() {
	local extracted=$temporary_work_dir/e2e-coverdata
	local profile=$temporary_work_dir/e2e-coverage.out

	mkdir -p "$extracted"
	if ! mcopy -i "$onboard_disk_image" -s '::/kobodeck-coverdata/*' "$extracted"; then
		die "cannot extract coverage data from onboard storage"
	fi
	if ! find "$extracted" -type f -name 'covmeta.*' -print -quit | grep -q .; then
		die "instrumentation produced no coverage metadata"
	fi
	if ! find "$extracted" -type f -name 'covcounters.*' -print -quit | grep -q .; then
		die "instrumentation produced no coverage counters"
	fi
	go tool covdata percent -i="$extracted" || die "coverage data is not usable"
	go tool covdata textfmt -i="$extracted" -o="$profile" ||
		die "cannot convert coverage data to text format"
	[[ -s "$profile" ]] || die "instrumentation produced an empty coverage profile"

	mkdir -p "$coverage_output_dir" "$(dirname "$coverage_profile")"
	cp -a "$extracted"/. "$coverage_output_dir"/
	cp "$profile" "$coverage_profile"
}

prepare_coverage_output() {
	mkdir -p "$coverage_output_dir"
	find "$coverage_output_dir" -maxdepth 1 -type f \
		\( -name 'covmeta.*' -o -name 'covcounters.*' \) -delete
	rm -f "$coverage_profile"
}

main() {
	if [[ -n "$coverage_enabled" ]]; then
		echo "vm test: coverage mode enabled"
	fi
	check_dependencies
	temporary_work_dir=$(mktemp -d)
	if [[ -n "$coverage_enabled" ]]; then
		prepare_coverage_output
	fi
	download_artifacts
	start_readeck
	prepare_readeck_test_data
	build_vm_filesystem
	run_vm
	if [[ -n "$coverage_enabled" ]]; then
		collect_coverage
	fi
	echo "vm test: PASS"
}

main "$@"
