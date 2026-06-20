#
# spec file for package vtessera
#
# Copyright (c) 2026 Vtessera contributors
#
# All modifications and additions to the file contributed by third parties
# remain under their original license.
#

%define rust_version 1.80
%define cargo_build_flags --release --locked --target x86_64-unknown-linux-musl

Name:           vtessera
Version:        0.1.0
Release:        0
Summary:        Opt-in compute marketplace metering daemon
License:        Apache-2.0
URL:            https://github.com/douglasdemaio/vtessera
Source0:        vtessera-%{version}.tar.xz
BuildRequires:  cargo >= %{rust_version}
BuildRequires:  rust >= %{rust_version}
BuildRequires:  rust-std-static-x86_64-unknown-linux-musl >= %{rust_version}
BuildRequires:  musl-tools
Requires:       systemd

%description
Vtessera is a metering daemon for the Vtessera compute marketplace.
It samples local resource usage from /proc, produces signed usage
receipts (Ed25519), and writes them to a state directory.

This is the v0 provider daemon — no inbound network or workload
execution. See the project repository for the full roadmap.

%prep
%setup -q

%build
cargo build %{cargo_build_flags}

%install
install -D -m 0755 target/x86_64-unknown-linux-musl/release/vtesserad \
  %{buildroot}%{_bindir}/vtesserad
install -D -m 0644 packaging/vtesserad.service \
  %{buildroot}%{_unitdir}/vtesserad.service
install -D -m 0644 packaging/vtessera.apparmor \
  %{buildroot}%{_sysconfdir}/apparmor.d/usr.bin.vtesserad
install -D -m 0644 packaging/vtessera.toml.example \
  %{buildroot}%{_sysconfdir}/vtessera/vtessera.toml.example

%pre
%service_add_pre vtesserad.service

%post
%service_add_post vtesserad.service

%preun
%service_del_preun vtesserad.service

%postun
%service_del_postun vtesserad.service

%files
%{_bindir}/vtesserad
%{_unitdir}/vtesserad.service
%{_sysconfdir}/apparmor.d/usr.bin.vtesserad
%{_sysconfdir}/vtessera/vtessera.toml.example

%doc README.md

%changelog
* Tue Jun 16 2026 Vtessera contributors
- Initial v0 release
