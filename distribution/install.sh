#!/bin/sh

# Monitorable agent installer
# Usage (preferred — keeps the key out of argv and out of sudo's auth.log). Get a root
# shell first, then export and pipe inside it — don't use sudo's -E flag: sudo-rs,
# Ubuntu's default sudo since 25.10, doesn't implement -E at all, and some sudoers
# policies refuse it even where it's implemented. A `VAR=x curl ... | sudo sh` prefix
# doesn't work either; it would apply to curl, which is the left side of the pipe, and
# never reach sudo.
#   sudo -s
#   export MONITORABLE_API_KEY=<key>
#   curl -fsSL @@BASE_URL@@/install.sh | sh -s -- --endpoint=<url>
# Fallback, when a root shell isn't available:
#   curl -fsSL @@BASE_URL@@/install.sh | sudo sh -s -- --api-key=<key>
# The fallback puts the key in the world-readable /proc/<pid>/cmdline for the whole run
# and in sudo's auth.log — prefer the root-shell form wherever you can.
# Optional: --endpoint=https://ingest.monitorable.net (default), --version=vX.Y.Z
# Update (key and endpoint from /etc/monitorable/agent.env): curl -fsSL @@BASE_URL@@/install.sh | sudo sh
# Uninstall: curl -fsSL @@BASE_URL@@/install.sh | sudo sh -s -- --uninstall

set -e

# Colors for output. Only when stdout is a terminal: a `curl | sudo sh` run captured to a
# log or driven by config management would otherwise get raw escape sequences.
if [ -t 1 ]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    BLUE='\033[0;34m'
    NC='\033[0m' # No Color
else
    RED=''
    GREEN=''
    YELLOW=''
    BLUE=''
    NC=''
fi

# Default values
# Defaults to the dedicated production ingest surface (the .net zone); the backend always
# passes --endpoint=, so this default only applies to a hand-run install. It applies only
# when neither a flag, the environment, nor an existing agent.env (a key-free re-run) gives
# an endpoint.
DEFAULT_ENDPOINT="https://ingest.monitorable.net"
# Empty unless set explicitly (env here, a flag below); a key-free re-run then takes the
# endpoint from agent.env before falling back to DEFAULT_ENDPOINT.
ENDPOINT="${MONITORABLE_ENDPOINT:-}"
AGENT_ENV="/etc/monitorable/agent.env"
# Prefer the key from the environment: an argv value sits in the world-readable
# /proc/<pid>/cmdline for the whole run and sudo writes it permanently to auth.log.
# --api-key= still works and overrides, for the command the dashboard renders today.
API_KEY="${MONITORABLE_API_KEY:-}"
INSTALL_DIR="/opt/monitorable"
CONFIG_DIR="/etc/monitorable"
BINARY_NAME="monitorable-agent"
SERVICE_NAME="monitorable-agent"
BASE_URL="@@BASE_URL@@"
VERSION="latest"
# Not USER/GROUP: those are exported by sudo and reassigning them would change $USER for
# the rest of this script and every child process.
SVC_USER="monitorable"
SVC_GROUP="monitorable"
UNINSTALL=0
OTHER_OPTS=0

printf '%b' "${BLUE}🚀 Monitorable agent installer${NC}\n"
printf '%b' "${BLUE}================================${NC}\n"

# Parse command line arguments
while [ $# -gt 0 ]; do
    case $1 in
        --endpoint=*)
            ENDPOINT="${1#*=}"
            OTHER_OPTS=1
            shift
            ;;
        --api-key=*)
            API_KEY="${1#*=}"
            OTHER_OPTS=1
            shift
            ;;
        --version=*)
            VERSION="${1#*=}"
            OTHER_OPTS=1
            shift
            ;;
        --uninstall)
            UNINSTALL=1
            shift
            ;;
        *)
            # %s, never %b: an argument is attacker-supplied in a pasted one-liner and %b
            # would let it emit escape sequences that forge or truncate installer output.
            printf '%b' "${RED}Unknown parameter:${NC} "
            printf '%s\n' "$1"
            exit 1
            ;;
    esac
done

if [ "$UNINSTALL" -eq 1 ] && [ "$OTHER_OPTS" -eq 1 ]; then
    printf '%b' "${RED}--uninstall takes no other options${NC}\n"
    exit 1
fi

# Check if running as root
if [ "$(id -u)" -ne 0 ]; then
   printf '%b' "${RED}This script must be run as root (use sudo)${NC}\n"
   exit 1
fi

if [ "$UNINSTALL" -eq 1 ]; then
    printf '%b' "${YELLOW}🧹 Uninstalling the Monitorable agent...${NC}\n"
    ACTED=0
    FAILED=0
    done_step() { printf '%b' "${GREEN}✓${NC} $1\n"; ACTED=1; }
    skip_step() { printf '%b' "· $1: not present\n"; }

    # monitorable-collector = the pre-rename unit (<= v1.1.x). disable --now runs
    # unconditionally for both names (harmless no-op on an unknown unit) so a unit whose
    # file was hand-deleted while it still runs gets stopped too; the file test below only
    # decides whether there's a file left to remove and which line to print.
    for unit in monitorable-agent monitorable-collector; do
        systemctl disable --now "$unit" >/dev/null 2>&1 || true
        if [ -f "/etc/systemd/system/$unit.service" ]; then
            if rm -f "/etc/systemd/system/$unit.service"; then
                done_step "Stopped and removed the $unit service"
            else
                printf '%b' "${YELLOW}⚠️  Could not remove the $unit service file${NC}\n"
                FAILED=1
            fi
        else
            skip_step "$unit service"
        fi
    done
    systemctl daemon-reload >/dev/null 2>&1 || true
    systemctl reset-failed monitorable-agent monitorable-collector >/dev/null 2>&1 || true

    # Binary, configuration (incl. the API key), state (incl. the unsent queue), logs.
    for dir in /opt/monitorable /etc/monitorable /var/lib/monitorable /var/log/monitorable; do
        if [ -e "$dir" ]; then
            if rm -rf "$dir"; then
                done_step "Deleted $dir"
            else
                printf '%b' "${YELLOW}⚠️  Could not delete $dir${NC}\n"
                FAILED=1
            fi
        else
            skip_step "$dir"
        fi
    done

    UDEV_RULE=/etc/udev/rules.d/99-monitorable-nvme-smart.rules
    if [ -f "$UDEV_RULE" ]; then
        if rm -f "$UDEV_RULE"; then
            if command -v udevadm >/dev/null 2>&1; then
                udevadm control --reload-rules >/dev/null 2>&1 || true
            fi
            done_step "Removed the NVMe SMART udev rule"
        else
            printf '%b' "${YELLOW}⚠️  Could not remove the NVMe SMART udev rule${NC}\n"
            FAILED=1
        fi
    else
        skip_step "NVMe SMART udev rule"
    fi

    if getent passwd "$SVC_USER" >/dev/null 2>&1; then
        if userdel "$SVC_USER" >/dev/null 2>&1; then
            done_step "Deleted the $SVC_USER user"
        else
            printf '%b' "${YELLOW}⚠️  Could not delete the $SVC_USER user (is a process still running as it?)${NC}\n"
            FAILED=1
        fi
    else
        skip_step "$SVC_USER user"
    fi
    if getent group "$SVC_GROUP" >/dev/null 2>&1; then
        if groupdel "$SVC_GROUP" >/dev/null 2>&1; then
            done_step "Deleted the $SVC_GROUP group"
        else
            printf '%b' "${YELLOW}⚠️  Could not delete the $SVC_GROUP group (is a process still running as it?)${NC}\n"
            FAILED=1
        fi
    fi

    printf '\n'
    if [ "$FAILED" -eq 1 ]; then
        printf '%b' "${RED}Uninstall incomplete: see the ⚠️ lines above.${NC}\n"
        exit 1
    fi
    if [ "$ACTED" -eq 1 ]; then
        printf '%b' "${GREEN}✅ Uninstalled. Now open the dashboard and click Remove for this server.${NC}\n"
    else
        printf '%b' "Nothing to remove: the Monitorable agent is not installed on this server.\n"
    fi
    exit 0
fi

# Key-free re-run = update: take what was not given explicitly from the agent.env this
# script wrote on the first install. Parsed, never sourced: the file is root-owned 0600,
# but a sourced file executes, and the values still go through the same checks below.
# A pre-v1.2.0 agent (monitorable-collector, v1.0.0–v1.1.23) has no agent.env: it kept
# both values as Environment= lines in its unit. Reading them there is a one-time
# migration — the install below writes agent.env and retires that unit.
LEGACY_UNIT="/etc/systemd/system/monitorable-collector.service"
file_value() {
    # $1 is one of two constant paths, $2 a constant line prefix; never user input.
    sed -n "s/^$2//p" "$1" | head -n 1
}
SOURCE_FILE=""
SOURCE_PREFIX=""
if [ -f "$AGENT_ENV" ]; then
    SOURCE_FILE="$AGENT_ENV"
elif [ -f "$LEGACY_UNIT" ]; then
    SOURCE_FILE="$LEGACY_UNIT"
    SOURCE_PREFIX="Environment="
fi
KEY_FROM_FILE=0
if [ -n "$SOURCE_FILE" ]; then
    if [ -z "$API_KEY" ]; then
        API_KEY="$(file_value "$SOURCE_FILE" "${SOURCE_PREFIX}MONITORABLE_API_KEY=")"
        [ -n "$API_KEY" ] && KEY_FROM_FILE=1
    fi
    if [ -z "$ENDPOINT" ]; then
        ENDPOINT="$(file_value "$SOURCE_FILE" "${SOURCE_PREFIX}MONITORABLE_ENDPOINT=")"
    fi
fi
[ -n "$ENDPOINT" ] || ENDPOINT="$DEFAULT_ENDPOINT"

if [ -z "$API_KEY" ]; then
    printf '%b' "${RED}No agent is installed on this server. Add the server in the dashboard to get its install command.${NC}\n"
    exit 1
fi
if [ "$KEY_FROM_FILE" -eq 1 ]; then
    if [ "$SOURCE_FILE" = "$AGENT_ENV" ]; then
        printf '%b' "${BLUE}🔁 Updating the existing agent (API key and endpoint from $AGENT_ENV)${NC}\n"
    else
        printf '%b' "${BLUE}🔁 Updating the existing agent (pre-v1.2.0: API key and endpoint from $LEGACY_UNIT; they move to $AGENT_ENV)${NC}\n"
    fi
fi

# The key and the endpoint are written verbatim into agent.env, and systemd hands every
# assignment in that file to the agent — a literal newline in either would append an
# arbitrary variable (SSL_CERT_FILE swaps Go's TLS root pool; MONITORABLE_STATE_DIR moves
# the queue). Keys are 64-char hex today, so the charset can be strict.
case "$API_KEY" in
    *[!A-Za-z0-9._-]*)
        printf '%b' "${RED}Error: the API key contains characters outside [A-Za-z0-9._-]${NC}\n"
        exit 1
        ;;
esac
# A URL legitimately needs more characters than a key, so reject only whitespace
# (space, tab, newline, carriage return) here.
case "$ENDPOINT" in
    *[[:space:]]*)
        printf '%b' "${RED}Error: the endpoint contains whitespace${NC}\n"
        exit 1
        ;;
esac

# Warn, do not reject: development installs legitimately point at http://ingest.monitorable.lan.
case "$ENDPOINT" in
    https://*) ;;
    *)
        printf '%b' "${RED}⚠️  WARNING: the endpoint is not https://${NC}\n"
        printf '%b' "${RED}   The API key and every metric will travel in cleartext — anyone on the${NC}\n"
        printf '%b' "${RED}   network path can read the key and modify what the agent reports.${NC}\n"
        printf '%b' "${RED}   Use an https:// endpoint unless this is a local development install.${NC}\n"
        ;;
esac

printf '%b' "${BLUE}Endpoint:${NC} "
printf '%s\n' "$ENDPOINT"
printf '%b' "${BLUE}Version:${NC} "
printf '%s\n' "$VERSION"
printf '\n'

# Detect OS and architecture
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

if [ "$OS" != "linux" ]; then
    printf '%b' "${RED}Unsupported OS:${NC} "
    printf '%s\n' "$OS"
    printf '%b' "This installer supports Linux only.\n"
    exit 1
fi

case $ARCH in
    x86_64|amd64)
        ARCH="amd64"
        ;;
    arm64|aarch64)
        ARCH="arm64"
        ;;
    *)
        printf '%b' "${RED}Unsupported architecture:${NC} "
        printf '%s\n' "$ARCH"
        exit 1
        ;;
esac

BINARY_FILE="$BINARY_NAME-linux-$ARCH"
CONFIG_FILE="collector-config.yaml"
UNIT_FILE="$SERVICE_NAME.service"

COLLECTOR_URL="$BASE_URL/binaries/otel/$VERSION/$BINARY_FILE"
SUMS_URL="$BASE_URL/binaries/otel/$VERSION/SHA256SUMS"
CONFIG_URL="$BASE_URL/configs/linux/$CONFIG_FILE"
SERVICE_FILE_URL="$BASE_URL/configs/linux/$UNIT_FILE"

printf '%b' "${BLUE}Detected platform:${NC} "
printf '%s\n' "$OS/$ARCH"
printf '%b' "${BLUE}Collector URL:${NC} "
printf '%s\n' "$COLLECTOR_URL"
printf '\n'

# Check if curl and sha256sum are available
if ! command -v curl >/dev/null 2>&1; then
    printf '%b' "${RED}curl is required but not installed.${NC}\n"
    printf '%b' "Please install curl and try again.\n"
    exit 1
fi
if ! command -v sha256sum >/dev/null 2>&1; then
    printf '%b' "${RED}sha256sum (coreutils) is required but not installed.${NC}\n"
    exit 1
fi
# systemd is the only supported init: the agent's security model is its sandboxed unit.
# Checked before ANY side effect, so an OpenRC host or a container without systemd gets a
# clear refusal instead of a half-install that already holds the API key.
if [ ! -d /run/systemd/system ] || ! command -v systemctl >/dev/null 2>&1; then
    printf '%b' "${RED}This installer requires systemd, which is not running on this host.${NC}\n"
    exit 1
fi

# Create installation and configuration directories. Only these two, before the download:
# the downloads are staged in them. The service user comes after verification, so a failed
# fresh install leaves no account (and no docker-group membership) behind.
printf '%b' "${YELLOW}📁 Creating directories...${NC}\n"
mkdir -p "$INSTALL_DIR" "$CONFIG_DIR"

# Set up directories and permissions
printf '%b' "${YELLOW}🔒 Setting up permissions...${NC}\n"

# Set ownership and permissions. $INSTALL_DIR is asserted explicitly, not left to the
# ambient umask: a pre-existing /opt/monitorable from an unrelated install could be owned
# by or writable to a non-root user, who would then control the binary that root's
# `systemctl restart` executes.
INSTALL_DIR_OWNER="$(stat -c '%U' "$INSTALL_DIR" 2>/dev/null || echo root)"
if [ "$INSTALL_DIR_OWNER" != "root" ]; then
    printf '%b' "${YELLOW}   $INSTALL_DIR was owned by ${NC}"
    printf '%s' "$INSTALL_DIR_OWNER"
    printf '%b' "${YELLOW} — re-owning it to root:root 755${NC}\n"
fi
chown root:root "$INSTALL_DIR"
chmod 755 "$INSTALL_DIR"
chown root:root "$CONFIG_DIR"
chmod 755 "$CONFIG_DIR"

# --- Download everything first, verify everything, install nothing yet -------------------
# All three files this installer puts on disk (binary, collector config, unit template) are
# listed in the ONE SHA256SUMS published next to the binaries, and all three are verified
# before anything is installed. The unit template especially: it is sed-rendered into
# /etc/systemd/system and daemon-reloaded, so an unverified one is an arbitrary ExecStart.
# Downloads land on .tmp paths under $INSTALL_DIR (root-owned; never /tmp, where a
# root-written file is a symlink-attack target) and are moved into place only at the end,
# so a failed download or a checksum mismatch leaves the previous install intact and
# running rather than taking the host down to no agent at all.
# What SHA256SUMS does NOT protect: it is fetched from the same bucket over the same TLS
# channel and is unsigned — see distribution/README.md.
printf '%b' "${YELLOW}📦 Downloading the Monitorable agent...${NC}\n"
TMP_BIN="$INSTALL_DIR/$BINARY_FILE.tmp"
TMP_SUMS="$INSTALL_DIR/SHA256SUMS.tmp"
TMP_UNIT="$INSTALL_DIR/$UNIT_FILE.tmp"
TMP_UNIT_SUBST="$INSTALL_DIR/$UNIT_FILE.subst.tmp"
# Staged on the SAME filesystem as their destination so the final `mv` is a rename and
# therefore atomic. Across a mount boundary — /opt is very often a separate one — mv
# degrades to create + copy, which is exactly the truncate-the-live-file failure mode the
# staging exists to prevent. Both directories are root-owned, so this is as safe as /opt;
# the unit is staged under a dot-name because systemd only reads *.service.
TMP_CONFIG="$CONFIG_DIR/$CONFIG_FILE.tmp"
TMP_UNIT_RENDERED="/etc/systemd/system/.$UNIT_FILE.tmp"

cleanup_tmp() {
    rm -f "$TMP_BIN" "$TMP_SUMS" "$TMP_CONFIG" "$TMP_UNIT" "$TMP_UNIT_SUBST" "$TMP_UNIT_RENDERED"
}
# Runs cleanup_tmp on any exit (error or otherwise) between now and the last install
# `mv` below, so an abort anywhere in the download/verify/install sequence (a failed
# download, a checksum mismatch, an interrupted script) leaves no `.tmp` files behind.
# Cleared once the installs land, below.
trap cleanup_tmp EXIT

# fetch <url> <dest>. An https origin stays https for the whole transfer: --proto also
# binds redirect targets, so a redirect cannot downgrade a download to cleartext, and TLS
# below 1.2 is refused. The dev origin (http://get.monitorable.lan) keeps plain curl.
fetch() {
    case "$BASE_URL" in
        https://*) curl --proto '=https' --tlsv1.2 -fsSL "$1" -o "$2" ;;
        *) curl -fsSL "$1" -o "$2" ;;
    esac
}
if ! fetch "$COLLECTOR_URL" "$TMP_BIN" ||
   ! fetch "$SUMS_URL" "$TMP_SUMS" ||
   ! fetch "$CONFIG_URL" "$TMP_CONFIG" ||
   ! fetch "$SERVICE_FILE_URL" "$TMP_UNIT"; then
    cleanup_tmp
    printf '%b' "${RED}❌ Failed to download the Monitorable agent distribution${NC}\n"
    exit 1
fi

# verify <downloaded path> <name as listed in SHA256SUMS>. Exact field match via awk, so a
# name that is a suffix of another cannot be confused; a missing entry leaves EXPECTED
# empty and fails closed. The config and the unit are UNVERSIONED objects while SHA256SUMS
# is published per version, so an older --version= whose sums predate the current config
# fails here instead of installing a mismatched pair.
verify() {
    _expected="$(awk -v n="$2" '$2 == n { print $1 }' "$TMP_SUMS")"
    _actual="$(sha256sum "$1" | cut -d' ' -f1)"
    if [ -z "$_expected" ] || [ "$_expected" != "$_actual" ]; then
        cleanup_tmp
        printf '%b' "${RED}❌ Checksum mismatch or missing SHA256SUMS entry for:${NC} "
        printf '%s\n' "$2"
        printf '%b' "${RED}   Nothing was installed; any existing agent is untouched.${NC}\n"
        exit 1
    fi
}
verify "$TMP_BIN" "$BINARY_FILE"
BIN_SHA256="$_actual"
verify "$TMP_CONFIG" "$CONFIG_FILE"
verify "$TMP_UNIT" "$UNIT_FILE"
rm -f "$TMP_SUMS"
printf '%b' "${GREEN}✅ Binary, config and unit template verified against SHA256SUMS${NC}\n"
# Named so the output records which bytes this host got; each release's SHA256SUMS is also
# attached to its GitHub release, an origin independent of this download host.
printf '   %s sha256 %s\n' "$BINARY_FILE" "$BIN_SHA256"

# Create user and group — only now that everything downloaded has verified.
printf '%b' "${YELLOW}👤 Creating user and group...${NC}\n"

# Create group if it doesn't exist
if ! getent group "$SVC_GROUP" >/dev/null 2>&1; then
    printf '%b' "Creating group: $SVC_GROUP\n"
    groupadd --system "$SVC_GROUP"
fi

# Create user if it doesn't exist
if ! getent passwd "$SVC_USER" >/dev/null 2>&1; then
    printf '%b' "Creating user: $SVC_USER\n"
    useradd --system --gid "$SVC_GROUP" --home-dir /var/lib/monitorable \
            --shell /sbin/nologin --comment "Monitorable Agent" "$SVC_USER"
fi

# Docker-group membership is root-equivalent (the socket starts privileged containers), so
# join it only on a host that actually runs Docker — the presence of a stale `docker` group
# is not a reason to grant it. A host that installs Docker later re-runs this installer.
if [ -S /var/run/docker.sock ] && getent group docker >/dev/null 2>&1; then
    printf '%b' "Adding $SVC_USER to docker group for Docker container metrics...\n"
    usermod -aG docker "$SVC_USER"
elif getent group docker | cut -d: -f4 | tr ',' '\n' | grep -qx "$SVC_USER"; then
    # No socket, but an earlier install left the membership behind (Docker removed, or the
    # socket moved). Drop it: it is root-equivalent and now buys nothing. The membership is
    # a supplementary one, so this never touches the user's primary group.
    printf '%b' "${YELLOW}Removing $SVC_USER from the docker group (no Docker socket on this host)${NC}\n"
    gpasswd -d "$SVC_USER" docker >/dev/null
fi

mkdir -p /var/lib/monitorable
chown "$SVC_USER:$SVC_GROUP" /var/lib/monitorable
chmod 755 /var/lib/monitorable

# SMART disk-health capabilities, tiered by detected hardware. NVMe needs CAP_SYS_ADMIN for
# the admin-passthrough ioctl; its controller char node /dev/nvmeX is 0600 root:root, so
# rather than grant the broad CAP_DAC_OVERRIDE we install a udev rule giving the disk group
# read access (the collector opens O_RDONLY) — least privilege. SATA/SAS gets CAP_SYS_RAWIO
# alone (its /dev/sd* block node is already disk-group readable). No physical disk → nothing.
# SMART_DEVICE_CLASSES additionally feeds the unit's DevicePolicy=closed allow-list: one
# read-only device class per node the SMART collector may open. Read-only is the point —
# the `disk` group's raw WRITE access to /dev/sda and /dev/nvme0n1 is what makes an agent
# compromise a host compromise, and DevicePolicy=closed removes it.
SMART_CAPS=""
SMART_GROUPS=""
SMART_DEVICE_CLASSES=""
# shellcheck disable=SC2010  # /sys/block entries are kernel device names (no globs/whitespace); ls|grep is safe and reads clearly here
if ls /sys/block 2>/dev/null | grep -q '^nvme'; then
    SMART_CAPS="CAP_SYS_RAWIO CAP_SYS_ADMIN"
    SMART_GROUPS="disk"
    # char-nvme (major 241) is the CONTROLLER node the reader opens — the nvme0n1 namespace
    # is block major 259 "blkext" and is never opened for SMART.
    SMART_DEVICE_CLASSES="char-nvme"
    printf '%b' "${BLUE}💽 NVMe detected — enabling SMART (CAP_SYS_RAWIO + CAP_SYS_ADMIN + disk-group udev rule)${NC}\n"
    # shellcheck disable=SC2010  # same as above
    if ls /sys/block 2>/dev/null | grep -q '^sd'; then
        # Mixed host: the collector enumerates sd* too, and without a read allowance the
        # open would fail EPERM and be reported as a SMART permission error.
        SMART_DEVICE_CLASSES="$SMART_DEVICE_CLASSES block-sd"
    fi
    # Grant the disk group read access to the NVMe controller char nodes so the
    # unprivileged collector can open them (O_RDONLY) without CAP_DAC_OVERRIDE;
    # CAP_SYS_ADMIN still gates the admin ioctl itself.
    if [ -d /etc/udev/rules.d ]; then
        printf '# Installed by the Monitorable agent. NVMe SMART/Identify are admin ioctls on\n# the controller node /dev/nvmeX (default 0600 root:root). Grant the disk group read\n# access so the unprivileged collector can open it O_RDONLY; CAP_SYS_ADMIN (from the\n# systemd unit) still gates the ioctl itself.\nSUBSYSTEM=="nvme", KERNEL=="nvme[0-9]*", GROUP="disk", MODE="0640"\n' \
            > /etc/udev/rules.d/99-monitorable-nvme-smart.rules
        udevadm control --reload-rules 2>/dev/null && udevadm trigger --subsystem-match=nvme 2>/dev/null || \
            printf '%b' "${YELLOW}   (udevadm unavailable — NVMe node permissions will apply on next reboot)${NC}\n"
    fi
elif ls /sys/block 2>/dev/null | grep -qE '^sd'; then
    SMART_CAPS="CAP_SYS_RAWIO"
    SMART_GROUPS="disk"
    SMART_DEVICE_CLASSES="block-sd"
    printf '%b' "${BLUE}💽 SATA/SAS detected — enabling SMART (CAP_SYS_RAWIO)${NC}\n"
else
    printf '%b' "${BLUE}💽 No physical disk detected — SMART disabled (no extra privileges)${NC}\n"
fi

# --- Install the verified artifacts ------------------------------------------------------
# Binary: chmod on the temp path then rename. On a re-run/upgrade the old binary may still
# be executing and the kernel refuses to overwrite a running file (ETXTBSY); a rename swaps
# the directory entry while the running process keeps its now-unlinked inode.
chmod 755 "$TMP_BIN"
mv -f "$TMP_BIN" "$INSTALL_DIR/$BINARY_NAME"
printf '%b' "${GREEN}✅ Monitorable agent installed${NC}\n"

# Config: permissions set on the temp path, then renamed over the live file, so a partial
# write can never leave the running agent with a truncated config to crash-loop on.
chown root:"$SVC_GROUP" "$TMP_CONFIG"
chmod 640 "$TMP_CONFIG"
mv -f "$TMP_CONFIG" "$CONFIG_DIR/$CONFIG_FILE"
printf '%b' "${GREEN}✅ Configuration file installed${NC}\n"

# Env file: API key and endpoint. Root-only, read by systemd via EnvironmentFile= before
# it drops privileges — the secret never appears in the (world-readable) unit file.
printf '%b' "${YELLOW}🔑 Writing agent.env...${NC}\n"
OLD_UMASK=$(umask)
umask 077
printf 'MONITORABLE_API_KEY=%s\nMONITORABLE_ENDPOINT=%s\n' "$API_KEY" "$ENDPOINT" > "$CONFIG_DIR/agent.env.tmp"
umask "$OLD_UMASK"
chown root:root "$CONFIG_DIR/agent.env.tmp" && chmod 600 "$CONFIG_DIR/agent.env.tmp"
mv -f "$CONFIG_DIR/agent.env.tmp" "$CONFIG_DIR/agent.env"

# systemd service setup
printf '%b' "${YELLOW}🔧 Setting up systemd service...${NC}\n"

# Render the verified unit template. The capability/group placeholders are single-valued,
# so sed handles them; the device placeholder expands to ZERO OR MORE lines, which a POSIX
# sed substitution cannot do, so awk emits one `DeviceAllow=<class> r` line per detected
# class — and drops the placeholder line entirely when none was detected. Rendered to a
# temp file first and renamed, so a failed render cannot truncate a live unit file. Two
# steps through a file, not a pipe: POSIX sh has no pipefail, so a failing sed on the left
# of a pipe would be masked by awk's exit status and escape set -e.
sed -e "s|__MONITORABLE_SMART_CAPS__|$SMART_CAPS|g" \
    -e "s|__MONITORABLE_SMART_GROUPS__|$SMART_GROUPS|g" \
    "$TMP_UNIT" > "$TMP_UNIT_SUBST"
awk -v classes="$SMART_DEVICE_CLASSES" '
    /__MONITORABLE_SMART_DEVICES__/ {
        n = split(classes, c, " ")
        for (i = 1; i <= n; i++) print "DeviceAllow=" c[i] " r"
        next
    }
    { print }
  ' "$TMP_UNIT_SUBST" > "$TMP_UNIT_RENDERED"
chown root:root "$TMP_UNIT_RENDERED"
chmod 644 "$TMP_UNIT_RENDERED"
mv -f "$TMP_UNIT_RENDERED" "/etc/systemd/system/$UNIT_FILE"
# All downloaded/verified artifacts are now installed; clear the trap so a later,
# unrelated failure (e.g. the service-start check below) doesn't re-run cleanup_tmp
# against paths that are already gone.
trap - EXIT
rm -f "$TMP_UNIT" "$TMP_UNIT_SUBST"

# Migrate an install made under the previous names (binary monitorable-otelcol, unit
# monitorable-collector). Removed only here — after the new binary is on disk and
# verified, and the new config/unit are installed — so a failed download or checksum
# mismatch earlier in the script leaves the previous install (if any) intact and running,
# instead of taking the host down to no agent at all. The one `systemctl daemon-reload`
# below picks up both this removal and the new unit.
# Idempotent: on a re-run the old unit file is already gone, so this is a no-op.
if [ -f /etc/systemd/system/monitorable-collector.service ]; then
    printf '%b' "${YELLOW}♻️  Removing the previous monitorable-collector unit...${NC}\n"
    systemctl disable --now monitorable-collector 2>/dev/null || true
    rm -f /etc/systemd/system/monitorable-collector.service
fi
rm -f "$INSTALL_DIR/monitorable-otelcol" "$INSTALL_DIR/monitorable-collector-run.sh"

# Enable and start service
printf '%b' "${YELLOW}▶️  Starting service...${NC}\n"
systemctl daemon-reload
systemctl enable "$SERVICE_NAME"
# Use restart, not start: on a re-run/upgrade the unit may already be active (or
# crash-looping), and `start` is a no-op on an active unit — the new binary/config
# would never load. reset-failed first clears any prior crash-loop counters so the
# NRestarts check below reflects only this (re)start, not stale history.
systemctl reset-failed "$SERVICE_NAME" 2>/dev/null || true
systemctl restart "$SERVICE_NAME"

# Verify the collector actually STAYS up. With Type=simple, systemd reports
# "active" the instant ExecStart forks — before the process can fail (bad config,
# missing capability) — so an immediate is-active check is unreliable. Wait for
# the unit to settle, then treat a non-active state OR any
# auto-restart (NRestarts > 0, i.e. it already crashed once) as a failed install.
sleep 4
NRESTARTS="$(systemctl show -p NRestarts --value "$SERVICE_NAME" 2>/dev/null || echo 0)"
if systemctl is-active --quiet "$SERVICE_NAME" && [ "${NRESTARTS:-0}" -eq 0 ]; then
    printf '%b' "${GREEN}✅ Service started successfully!${NC}\n"
else
    printf '%b' "${RED}❌ The agent failed to start and is restarting in a loop.${NC}\n"
    printf '%b' "${YELLOW}Recent logs:${NC}\n"
    journalctl -u "$SERVICE_NAME" -n 20 --no-pager 2>/dev/null || true
    printf '%b' "\n${YELLOW}Follow the logs with:${NC}\n"
    printf '%b' "   journalctl -u $SERVICE_NAME -f\n"
    exit 1
fi

printf '\n'
printf '%b' "${GREEN}🎉 Installation complete!${NC}\n"
printf '\n'
printf '%b' "Service status:\n"
systemctl status "$SERVICE_NAME" --no-pager -l || true

printf '\n'
printf '%b' "${BLUE}📡 The agent is now sending metrics to:${NC}\n"
printf '   %s\n' "$ENDPOINT/v1/metrics"
printf '\n'
printf '%b' "${BLUE}📋 To view logs:${NC}\n"
printf '%b' "   journalctl -u $SERVICE_NAME -f\n"
printf '\n'
printf '%b' "${BLUE}⏹️  To stop the service:${NC}\n"
printf '%b' "   systemctl stop $SERVICE_NAME\n"
printf '\n'
printf '%b' "${BLUE}🔄 To restart the service:${NC}\n"
printf '%b' "   systemctl restart $SERVICE_NAME\n"
printf '\n'
printf '%b' "${GREEN}🔐 Security: the agent runs as the dedicated, unprivileged '$SVC_USER' user under a${NC}\n"
printf '%b' "${GREEN}   sandboxed systemd unit (read-only device policy, syscall filter, no capabilities${NC}\n"
printf '%b' "${GREEN}   beyond the SMART ioctls your hardware needs).${NC}\n"

printf '\n'
printf '%b' "${BLUE}📚 For more information and troubleshooting:${NC}\n"
printf '%b' "   https://monitorable.io/docs/collector/install/\n"
printf '\n'
printf '%b' "${GREEN}🎯 Welcome to OpenTelemetry-native monitoring with Monitorable!${NC}\n"
