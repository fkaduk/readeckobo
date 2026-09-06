#!/bin/sh
set -u

# Guest-side half of the end-to-end test.
# The host passes this script to the kernel as rdinit,
# so it runs as PID 1 in a minimal initramfs.

busybox=/bin/busybox
log=/mnt/onboard/.adds/kobodeck/kobodeck.log
nickel_status=/tmp/nickel-hardware-status
binary=/usr/local/bin/kobodeck
rule=/etc/udev/rules.d/90-kobodeck.rules
coverage_dir=
coverage_counter_count=0

fail() {
	echo "KOBODECK_VM_ERROR: $*"
	if [ -f "$log" ]; then
		echo "KOBODECK_VM_LOG_BEGIN"
		cat "$log"
		echo "KOBODECK_VM_LOG_END"
	fi
	poweroff -f
	while :; do
		sleep 3600
	done
}

install_busybox() {
	echo "KOBODECK_VM: installing BusyBox applets"
	"$busybox" --install -s
	export PATH=/usr/bin:/bin:/usr/sbin:/sbin
}

mount_virtual_filesystems() {
	# Script runs as PID=1, so it must mount the usual kernel-provided
	# filesystems itself.
	echo "KOBODECK_VM: mounting virtual filesystems"
	mount -t devtmpfs devtmpfs /dev || fail "mount devtmpfs"
	mount -t proc proc /proc || fail "mount proc"
	mount -t sysfs sysfs /sys || fail "mount sysfs"
	mkdir -p /tmp
}

configure_network() {
	echo "KOBODECK_VM: configuring devices and network"
	modprobe -a virtio_mmio virtio_net virtio_blk af_packet fat vfat nls_cp437 nls_iso8859_1 ||
		fail "load kernel modules"
	mdev -s
	ip link set lo up || fail "enable loopback"
	ip link set eth0 up || fail "enable eth0"
	udhcpc -q -n -t 10 -i eth0 -s /usr/share/udhcpc/default.script ||
		fail "obtain DHCP lease"
}

mount_onboard_storage() {
	echo "KOBODECK_VM: setting up onboard storage mount"
	mkdir -p /mnt/onboard
	mount -t vfat /dev/vda /mnt/onboard || fail "mount onboard FAT storage"
}

install_test_payload() {
	echo "KOBODECK_VM: installing test dependencies and release"
	apk add --initdb eudev sqlite || fail "install eudev and sqlite"
	tar -xzf /test/KoboRoot.tgz -C / || fail "install KoboRoot.tgz"
	if [ -f /test/90-kobodeck-cover.rules ]; then
		echo "KOBODECK_VM: enabling test-only coverage collection"
		coverage_dir=/mnt/onboard/kobodeck-coverdata
		mkdir -p "$coverage_dir" || fail "create coverage directory"
		cp /test/90-kobodeck-cover.rules "$rule" || fail "install coverage udev rule"
	fi
	mkdir -p /mnt/onboard/.adds/kobodeck /mnt/onboard/.kobo
	cp /test/kobodeck.toml /mnt/onboard/.adds/kobodeck/kobodeck.toml ||
		fail "install Kobodeck configuration"
	sqlite3 /mnt/onboard/.kobo/KoboReader.sqlite </test/nickel-schema.sql ||
		fail "create Nickel database"
	touch "$nickel_status" || fail "create Nickel status file"
}

count_coverage_files() {
	find "$coverage_dir" -maxdepth 1 -type f -name "$1" | wc -l
}

verify_coverage_files() {
	stage=$1
	minimum_counters=$2
	waited=0
	while :; do
		metadata_count=$(count_coverage_files 'covmeta.*')
		counter_count=$(count_coverage_files 'covcounters.*')
		if [ "$metadata_count" -gt 0 ] && [ "$counter_count" -ge "$minimum_counters" ]; then
			coverage_counter_count=$counter_count
			return
		fi
		waited=$((waited + 1))
		[ "$waited" -lt 10 ] ||
			fail "coverage files missing after $stage: metadata=$metadata_count counters=$counter_count"
		sleep 1
	done
}

start_udev() {
	echo "KOBODECK_VM: starting udev with a wlan interface"
	ip link set eth0 down || fail "disable eth0"
	ip link set eth0 name wlan0 || fail "rename network interface"
	ip link set wlan0 up || fail "enable wlan0"
	mkdir -p /run/udev
	udevd --daemon || fail "start udev"
	[ ! -e "$log" ] || fail "Kobodeck ran before a Wi-Fi event"
}

verify_remove_event() {
	echo "KOBODECK_VM: verifying remove event is ignored"
	udevadm trigger --action=remove /sys/class/net/wlan0 || fail "trigger remove event"
	sleep 1
	[ ! -e "$log" ] || fail "remove event unexpectedly ran Kobodeck"
}

verify_synchronization() {
	echo "KOBODECK_VM: triggering Kobodeck through installed udev rule"
	udevadm trigger --action=add /sys/class/net/wlan0 || fail "trigger add event"

	echo "KOBODECK_VM: waiting for Kobodeck to complete"
	waited=0
	while ! grep -q ' completed in ' "$log" 2>/dev/null; do
		waited=$((waited + 1))
		[ "$waited" -lt 60 ] || fail "Kobodeck did not complete"
		sleep 1
	done

	echo "KOBODECK_VM: verifying synchronization results"
	bookmark_id=$(cat /test/bookmark-id)
	download=/mnt/onboard/kobodeck/$bookmark_id.kepub.epub
	[ -s "$download" ] || fail "downloaded KEPUB is missing"

	epub_mimetype=$(unzip -p "$download" mimetype 2>/dev/null) ||
		fail "downloaded KEPUB is not a readable ZIP archive"
	[ "$epub_mimetype" = "application/epub+zip" ] ||
		fail "downloaded KEPUB has an invalid mimetype"
	unzip -p "$download" META-INF/container.xml >/dev/null 2>&1 ||
		fail "downloaded KEPUB has no container metadata"

	grep -Fq 'usb plug add' "$nickel_status" || fail "Nickel add event is missing"
	grep -Fq 'usb plug remove' "$nickel_status" || fail "Nickel remove event is missing"
	if [ -n "$coverage_dir" ]; then
		verify_coverage_files synchronization 1
	fi
}

verify_uninstall() {
	echo "KOBODECK_VM: triggering and verifying uninstall through udev"
	: >/mnt/onboard/.adds/kobodeck/kobodeck.toml
	udevadm trigger --action=add /sys/class/net/wlan0 || fail "trigger uninstall event"
	waited=0
	while [ -e "$binary" ] || [ -e "$rule" ] || [ -e /mnt/onboard/.adds/kobodeck ]; do
		waited=$((waited + 1))
		[ "$waited" -lt 30 ] || fail "Kobodeck did not uninstall"
		sleep 1
	done
	[ -s "$download" ] || fail "uninstall removed downloaded article"
	if [ -n "$coverage_dir" ]; then
		verify_coverage_files uninstall $((coverage_counter_count + 1))
	fi
}

shutdown() {
	if [ -n "$coverage_dir" ]; then
		echo "KOBODECK_VM: syncing coverage data to onboard storage"
		sync || fail "sync onboard storage"
	fi
	echo "KOBODECK_VM_PASS"
	poweroff -f
	fail "power off"
}

main() {
	install_busybox
	mount_virtual_filesystems
	configure_network
	mount_onboard_storage
	install_test_payload
	start_udev
	verify_remove_event
	verify_synchronization
	verify_uninstall
	shutdown
}

main "$@"
