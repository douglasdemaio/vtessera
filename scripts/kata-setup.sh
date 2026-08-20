#!/usr/bin/env bash
set -euo pipefail

# Vtessera Kata Containers setup script
# Installs and configures Kata Containers with Cloud Hypervisor on fresh nodes.
#
# Usage:
#   kata-setup.sh --install    # Install all Kata dependencies
#   kata-setup.sh --check      # Verify all components are installed
#   kata-setup.sh --uninstall  # Remove Kata components (optional)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

# Default versions
KATA_VERSION="${KATA_VERSION:-3.12.0}"
CH_VERSION="${CH_VERSION:-44.0}"
CNI_VERSION="${CNI_VERSION:-1.6.2}"
VIRTIOFSD_VERSION="${VIRTIOFSD_VERSION:-0.13.1}"

# Installation paths
KATA_INSTALL_DIR="/opt/kata"
CONTAINERD_CONFIG="/etc/containerd/config.toml"
KATA_CONFIG="/opt/kata/share/defaults/kata-containers/configuration.toml"

log() {
    echo "[kata-setup] $*"
}

error() {
    echo "[kata-setup] ERROR: $*" >&2
    exit 1
}

check_root() {
    if [[ $EUID -ne 0 ]]; then
        error "This script must be run as root"
    fi
}

check_system() {
    if ! command -v uname &>/dev/null; then
        error "uname not found"
    fi

    local arch
    arch=$(uname -m)
    case "$arch" in
        x86_64) ARCH="amd64" ;;
        aarch64) ARCH="arm64" ;;
        *) error "Unsupported architecture: $arch" ;;
    esac

    local os
    os=$(uname -s)
    if [[ "$os" != "Linux" ]]; then
        error "This script only supports Linux"
    fi

    log "Detected system: $os $arch"
}

install_containerd() {
    log "Installing containerd..."

    if command -v containerd &>/dev/null; then
        log "containerd already installed: $(containerd --version)"
        return 0
    fi

    # Try zypper first (openSUSE), then apt (Debian/Ubuntu)
    if command -v zypper &>/dev/null; then
        zypper install -y containerd
    elif command -v apt-get &>/dev/null; then
        apt-get update
        apt-get install -y containerd.io
    else
        error "No supported package manager found. Install containerd manually."
    fi

    log "containerd installed: $(containerd --version)"
}

install_kata_runtime() {
    log "Installing kata-shim-v2..."

    if command -v kata-runtime &>/dev/null; then
        log "kata-runtime already installed: $(kata-runtime --version 2>&1 | head -1)"
        return 0
    fi

    local kata_url="https://github.com/kata-containers/kata-containers/releases/download/${KATA_VERSION}/kata-static-${KATA_VERSION}-linux-${ARCH}.tar.xz"

    mkdir -p "$KATA_INSTALL_DIR"
    curl -fsSL "$kata_url" | tar -xJ -C /

    # Create symlinks
    ln -sf "${KATA_INSTALL_DIR}/bin/kata-runtime" /usr/local/bin/kata-runtime
    ln -sf "${KATA_INSTALL_DIR}/bin/kata-shim-v2" /usr/local/bin/kata-shim-v2
    ln -sf "${KATA_INSTALL_DIR}/bin/kata-collect-data.sh" /usr/local/bin/kata-collect-data.sh

    log "kata-shim-v2 installed: ${KATA_VERSION}"
}

install_cloud_hypervisor() {
    log "Installing Cloud Hypervisor..."

    if command -v cloud-hypervisor &>/dev/null; then
        log "Cloud Hypervisor already installed: $(cloud-hypervisor --version)"
        return 0
    fi

    local ch_url="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v${CH_VERSION}/cloud-hypervisor-${ARCH}"

    curl -fsSL "$ch_url" -o /usr/local/bin/cloud-hypervisor
    chmod +x /usr/local/bin/cloud-hypervisor

    log "Cloud Hypervisor installed: v${CH_VERSION}"
}

install_virtiofsd() {
    log "Installing virtiofsd..."

    if command -v virtiofsd &>/dev/null; then
        log "virtiofsd already installed"
        return 0
    fi

    local vhost_url="https://github.com/cloud-hypervisor/virtiofsd/releases/download/v${VIRTIOFSD_VERSION}/virtiofsd-${ARCH}"
    local vhost_path="/usr/libexec/virtiofsd"

    mkdir -p "$(dirname "$vhost_path")"
    curl -fsSL "$vhost_url" -o "$vhost_path"
    chmod +x "$vhost_path"

    log "virtiofsd installed: v${VIRTIOFSD_VERSION}"
}

install_cni_plugins() {
    log "Installing CNI plugins..."

    if command -v cnitool &>/dev/null; then
        log "CNI plugins already installed"
        return 0
    fi

    local cni_url="https://github.com/containernetworking/plugins/releases/download/v${CNI_VERSION}/cni-plugins-linux-${ARCH}-v${CNI_VERSION}.tgz"

    mkdir -p /opt/cni/bin
    curl -fsSL "$cni_url" | tar -xz -C /opt/cni/bin

    log "CNI plugins installed: v${CNI_VERSION}"
}

configure_containerd() {
    log "Configuring containerd for Kata..."

    if [[ ! -f "$CONTAINERD_CONFIG" ]]; then
        error "containerd config not found at $CONTAINERD_CONFIG"
    fi

    # Backup original config
    if [[ ! -f "${CONTAINERD_CONFIG}.bak" ]]; then
        cp "$CONTAINERD_CONFIG" "${CONTAINERD_CONFIG}.bak"
    fi

    # Check if Kata runtime is already configured
    if grep -q "kata" "$CONTAINERD_CONFIG"; then
        log "containerd already configured for Kata"
        return 0
    fi

    # Add Kata runtime configuration
    cat >> "$CONTAINERD_CONFIG" << 'EOF'

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata]
  runtime_type = "io.containerd.kata.v2"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration.toml"
EOF

    log "containerd configured for Kata runtime"
}

configure_kata() {
    log "Configuring Kata Containers..."

    if [[ ! -d "$KATA_INSTALL_DIR" ]]; then
        error "Kata installation not found at $KATA_INSTALL_DIR"
    fi

    # Generate Kata configuration.toml with Cloud Hypervisor
    cat > "$KATA_CONFIG" << 'EOF'
# Kata Containers configuration for Vtessera
# Uses Cloud Hypervisor as the hypervisor

[hypervisor.clh]
path = "/usr/local/bin/cloud-hypervisor"
kernel = "/opt/kata/share/kata-containers/vmlinuz.container"
image = "/opt/kata/share/kata-containers/kata-containers.img"
initrd = "/opt/kata/share/kata-containers/kata-containers-initrd.img"
machine_type = "q35"
default_vcpus = 1
default_memory = 2048
disable_block_device_use = false
shared_fs = "virtio-fs"
virtio_fs_daemon = "/usr/libexec/virtiofsd"
enable_annotations = ["enable_iommu", "default_vcpus", "default_memory"]
valid_hypervisor_paths = ["/usr/local/bin/cloud-hypervisor"]
kernel_params = "agent.log_vport=1024 agent.debug=false"
kernel_modules = []
disable_new_netns = false
internetworking_model = "tcfilter"
disable_guest_seccomp = true
sandbox_bind_mounts = []
vfio_mode = "guest-kernel"

[agent.kata]
kernel_modules = []
enable_tracing = false
external_agent_connect_attempts = 10
dial_timeout = 60

[runtime]
internetworking_model = "tcfilter"
disable_guest_seccomp = true
sandbox_bind_mounts = []
vfio_mode = "guest-kernel"
EOF

    log "Kata configured with Cloud Hypervisor"
}

setup_gpu_support() {
    log "Setting up GPU support..."

    # Ensure vfio-pci module is available
    if ! lsmod | grep -q vfio_pci; then
        modprobe vfio_pci
    fi

    # Create udev rule for VFIO devices
    if [[ ! -f /etc/udev/rules.d/99-vtessera-vfio.rules ]]; then
        cat > /etc/udev/rules.d/99-vtessera-vfio.rules << 'EOF'
# Vtessera VFIO GPU passthrough
SUBSYSTEM=="vfio", KERNEL=="vfio", MODE="0660", GROUP="kvm"
SUBSYSTEM=="vfio", KERNEL=="[0-9]*", MODE="0660", GROUP="kvm"
EOF
        udevadm control --reload-rules
        udevadm trigger
    fi

    log "GPU support configured"
}

start_services() {
    log "Starting services..."

    # Restart containerd
    if systemctl is-active --quiet containerd; then
        systemctl restart containerd
        log "containerd restarted"
    else
        systemctl start containerd
        systemctl enable containerd
        log "containerd started and enabled"
    fi

    log "Services started"
}

verify_installation() {
    log "Verifying installation..."

    local errors=0

    # Check containerd
    if command -v containerd &>/dev/null; then
        log "✓ containerd: $(containerd --version)"
    else
        log "✗ containerd: not found"
        ((errors++))
    fi

    # Check kata-runtime
    if command -v kata-runtime &>/dev/null; then
        log "✓ kata-runtime: $(kata-runtime --version 2>&1 | head -1)"
    else
        log "✗ kata-runtime: not found"
        ((errors++))
    fi

    # Check kata-shim-v2
    if command -v kata-shim-v2 &>/dev/null; then
        log "✓ kata-shim-v2: available"
    else
        log "✗ kata-shim-v2: not found"
        ((errors++))
    fi

    # Check Cloud Hypervisor
    if command -v cloud-hypervisor &>/dev/null; then
        log "✓ cloud-hypervisor: $(cloud-hypervisor --version)"
    else
        log "✗ cloud-hypervisor: not found"
        ((errors++))
    fi

    # Check virtiofsd
    if command -v virtiofsd &>/dev/null || [[ -x /usr/libexec/virtiofsd ]]; then
        log "✓ virtiofsd: available"
    else
        log "✗ virtiofsd: not found"
        ((errors++))
    fi

    # Check /dev/kvm
    if [[ -e /dev/kvm ]]; then
        log "✓ /dev/kvm: available"
    else
        log "✗ /dev/kvm: not found"
        ((errors++))
    fi

    # Check containerd is running
    if systemctl is-active --quiet containerd; then
        log "✓ containerd service: running"
    else
        log "✗ containerd service: not running"
        ((errors++))
    fi

    if [[ $errors -eq 0 ]]; then
        log "All checks passed!"
        return 0
    else
        log "Some checks failed ($errors errors)"
        return 1
    fi
}

uninstall() {
    log "Uninstalling Kata Containers..."

    # Stop services
    systemctl stop containerd 2>/dev/null || true
    systemctl disable containerd 2>/dev/null || true

    # Remove containerd config
    if [[ -f "${CONTAINERD_CONFIG}.bak" ]]; then
        mv "${CONTAINERD_CONFIG}.bak" "$CONTAINERD_CONFIG"
    fi

    # Remove Kata installation
    rm -rf "$KATA_INSTALL_DIR"
    rm -f /usr/local/bin/kata-runtime
    rm -f /usr/local/bin/kata-shim-v2
    rm -f /usr/local/bin/kata-collect-data.sh

    # Remove Cloud Hypervisor
    rm -f /usr/local/bin/cloud-hypervisor

    # Remove virtiofsd
    rm -f /usr/libexec/virtiofsd

    # Remove CNI plugins
    rm -rf /opt/cni/bin

    # Remove udev rule
    rm -f /etc/udev/rules.d/99-vtessera-vfio.rules
    udevadm control --reload-rules

    log "Kata Containers uninstalled"
}

usage() {
    cat << EOF
Vtessera Kata Containers Setup

Usage: $0 [OPTIONS]

Options:
  --install     Install all Kata dependencies
  --check       Verify all components are installed
  --uninstall   Remove Kata components
  --help        Show this help message

Environment variables:
  KATA_VERSION       Kata Containers version (default: $KATA_VERSION)
  CH_VERSION         Cloud Hypervisor version (default: $CH_VERSION)
  CNI_VERSION        CNI plugins version (default: $CNI_VERSION)
  VIRTIOFSD_VERSION  virtiofsd version (default: $VIRTIOFSD_VERSION)

Examples:
  $0 --install          # Install everything
  $0 --check            # Verify installation
  KATA_VERSION=3.11.0 $0 --install  # Install specific version
EOF
}

main() {
    local action=""

    while [[ $# -gt 0 ]]; do
        case $1 in
            --install) action="install"; shift ;;
            --check) action="check"; shift ;;
            --uninstall) action="uninstall"; shift ;;
            --help) usage; exit 0 ;;
            *) error "Unknown option: $1" ;;
        esac
    done

    if [[ -z "$action" ]]; then
        usage
        exit 1
    fi

    case $action in
        install)
            check_root
            check_system
            install_containerd
            install_kata_runtime
            install_cloud_hypervisor
            install_virtiofsd
            install_cni_plugins
            configure_containerd
            configure_kata
            setup_gpu_support
            start_services
            verify_installation
            ;;
        check)
            check_system
            verify_installation
            ;;
        uninstall)
            check_root
            uninstall
            ;;
    esac
}

main "$@"
