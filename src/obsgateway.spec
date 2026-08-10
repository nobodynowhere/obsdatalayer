%global debug_package %{nil}

Name:		 obsgateway
Version: 26.7.2
Release: 1%{?dist}
Summary: Observability Data Layer Gateway
License: Apache-2.0
URL:     https://github.com/joellarkin/obsgateway
Provides: obsgateway-linux-amd64

Source0: %{name}-linux-amd64
Source1: %{name}.service
Source2: %{name}.default
Source3: %{name}.yml

%{?systemd_requires}
%if 0%{?fedora} >= 19
BuildRequires: systemd-rpm-macros
%endif
Requires(pre): shadow-utils

%description

This is a Go-based observability data-layer gateway. It sits in front of 
Loki (logs), Mimir (metrics), and Tempo (traces) and exposes a single 
HTTP API that proxies/rewrites push and query traffic, injects tenant 
headers, and can fan writes out to multiple backends.

%build
/bin/true

%install
mkdir -vp %{buildroot}%{_sysconfdir}/obsgateway
install -D -m 755 %{SOURCE0} %{buildroot}%{_bindir}/obsgateway-linux-amd64
install -D -m 644 %{SOURCE1} %{buildroot}%{_unitdir}/obsgateway.service
install -D -m 644 %{SOURCE2} %{buildroot}%{_sysconfdir}/default/obsgateway
install -D -m 644 %{SOURCE3} %{buildroot}%{_sysconfdir}/obsgateway/obsgateway.yml

%pre
getent group obsgateway >/dev/null || groupadd -r obsgateway
getent passwd obsgateway >/dev/null || \
  useradd -r -g obsgateway -d %{_sysconfdir}/obsgateway -s /sbin/nologin \
          -c "obsgateway service" obsgateway
exit 0

%post
%systemd_post obsgateway.service

%preun
%systemd_preun obsgateway.service

%postun
%systemd_postun obsgateway.service

%files
%defattr(-,root,root,-)
%{_bindir}/obsgateway-linux-amd64
%{_unitdir}/obsgateway.service
%config(noreplace) %{_sysconfdir}/default/obsgateway
%config(noreplace) %{_sysconfdir}/obsgateway/obsgateway.yml
