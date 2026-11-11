; crispy-companion.nsi -- Windows installer for the Crispy Companion daemon.
;
; Per-user by design: the daemon is loopback-only and everything it owns
; lives inside the user's own profile, so there is nothing to elevate for.
; RequestExecutionLevel user keeps it that way -- no UAC prompt, and it can
; install on a locked-down workstation.
;
; What it lays down (see packaging/windows/README.md):
;   %LOCALAPPDATA%\Programs\Crispy Companion\
;       crispy-companion.exe   the daemon
;       torrserver.exe         pinned MatriX engine, sha256-checked in CI
;       config.toml            seed copied into the data dir on first run
;       crispy-companion.ico   the same icon the macOS and Linux builds ship
;       LICENSE.txt            GPL-3.0-only
;       uninstall.exe
;   %LOCALAPPDATA%\Crispy Companion\
;       the daemon's data dir (tokens, pairing state, engine cache) and the
;       seed config's home -- the daemon creates it itself as well
;   Start Menu\Programs\Crispy Companion.lnk
;       --menu-launch entry: the headless daemon's only window
;
; Autostart is deliberately NOT written here. The daemon writes the HKCU
; Run value itself on the first start (internal/autostart), exactly as it
; does when run from an extracted zip, so the installer cannot drift from
; what the daemon does. The uninstaller removes the value either way, so
; nothing survives an uninstall.
;
; Build (CI passes the version, the staging dir and the output name):
;   makensis -NOCD -DVERSION=0.1.0 -DOUTFILE=setup.exe \
;            packaging/windows/crispy-companion.nsi
; -NOCD is required: makensis changes its own working directory to this
; script's folder by default, and every ${__FILEDIR__} path below is
; relative to the repository root (where the staging drop-in lives).
; Without -DSTAGEDIR the three payload files are expected in a "stage"
; folder next to this script, so a packaging/windows/stage drop-in builds
; locally with no flags.

Unicode true
ManifestDPIAware true

!ifndef APPNAME
!define APPNAME "Crispy Companion"
!endif
!ifndef COMPANYNAME
!define COMPANYNAME "Crispy Contributors"
!endif
!ifndef EXENAME
!define EXENAME "crispy-companion.exe"
!endif
!ifndef ENGINENAME
!define ENGINENAME "torrserver.exe"
!endif
!ifndef ICONNAME
!define ICONNAME "crispy-companion.ico"
!endif
!ifndef VERSION
!define VERSION "0.1.0"
!endif
!ifndef OUTFILE
!define OUTFILE "crispy-companion-setup.exe"
!endif
!ifndef STAGEDIR
!define STAGEDIR "${__FILEDIR__}\stage"
!endif

; The registry keys this installer owns. The uninstall key is the one
; Windows itself reads for Settings -> Apps -> Installed apps, so the entry
; and its uninstaller stay discoverable like any other per-user install.
!define INSTALLKEY "Software\${APPNAME}"
!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}"
; The autostart Run value the daemon writes; the uninstaller removes it.
!define RUNKEY "Software\Microsoft\Windows\CurrentVersion\Run"
!define RUNVALUE "Crispy Companion"

; The start-menu entry answers a click with the Crispy web app, since a
; headless daemon has no window of its own to show.
!define MENUPARAMS "--menu-launch"

Name "${APPNAME}"
OutFile "${OUTFILE}"
InstallDir "$LOCALAPPDATA\Programs\${APPNAME}"
; Remember the chosen folder per user; an upgrade lands in the same place
; as the install it updates, which the HKCU Run value depends on.
InstallDirRegKey HKCU "${INSTALLKEY}" "InstallDir"
RequestExecutionLevel user
SetCompressor /SOLID lzma

; Three numeric fields: CI passes -DVERSION=0.1.0, anything with letters in
; it (a sha, "-dirty", a pre-release suffix) is rejected here.
VIProductVersion ${VERSION}.0
VIAddVersionKey "ProductName" "${APPNAME}"
VIAddVersionKey "FileDescription" "${APPNAME} setup"
VIAddVersionKey "CompanyName" "${COMPANYNAME}"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "LegalCopyright" "GPL-3.0-only"
VIAddVersionKey "OriginalFilename" "${OUTFILE}"

!include "MUI2.nsh"
!include "LogicLib.nsh"

; The app icon everywhere a shell shows this installer: window, welcome
; and finish pages, installer/uninstaller binaries and the Start Menu
; entry.
!define MUI_ICON "${__FILEDIR__}\icons\${ICONNAME}"
!define MUI_UNICON "${__FILEDIR__}\icons\${ICONNAME}"
!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_RUN "$INSTDIR\${EXENAME}"
!define MUI_FINISHPAGE_RUN_PARAMETERS "${MENUPARAMS}"
!define MUI_FINISHPAGE_RUN_TEXT "Start Crispy Companion and open the web app"
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

BrandingText "${APPNAME} ${VERSION}"
ShowInstDetails hide
ShowUnInstDetails hide

Section "-Payload"
	SetOutPath "$INSTDIR"
	File "${STAGEDIR}\${EXENAME}"
	File "${STAGEDIR}\${ENGINENAME}"
	File "${STAGEDIR}\config.toml"
	File "${STAGEDIR}\icons\${ICONNAME}"
	File "/oname=LICENSE.txt" "${__FILEDIR__}\..\..\LICENSE"
	WriteUninstaller "$INSTDIR\uninstall.exe"
SectionEnd

Section "-Start menu entry"
	CreateDirectory "$SMPROGRAMS"
	CreateShortCut "$SMPROGRAMS\${APPNAME}.lnk" "$INSTDIR\${EXENAME}" \
		"${MENUPARAMS}" "$INSTDIR\${ICONNAME}" 0 "" "" \
		"${APPNAME}: open the Crispy web app"
SectionEnd

Section "-Data dir and seed config"
	CreateDirectory "$LOCALAPPDATA\${APPNAME}"
	; Never overwrite a config the user edited: the seed only lands on a
	; first install, upgrades keep whatever is already there.
	${If} ${FileExists} "$LOCALAPPDATA\${APPNAME}\config.toml"
	${Else}
		CopyFiles /SILENT "$INSTDIR\config.toml" "$LOCALAPPDATA\${APPNAME}"
	${EndIf}
SectionEnd

Section "-Programs entry"
	WriteRegStr HKCU "${UNINSTKEY}" "DisplayName" "${APPNAME}"
	WriteRegStr HKCU "${UNINSTKEY}" "DisplayIcon" "$INSTDIR\${ICONNAME}"
	WriteRegStr HKCU "${UNINSTKEY}" "DisplayVersion" "${VERSION}"
	WriteRegStr HKCU "${UNINSTKEY}" "Publisher" "${COMPANYNAME}"
	WriteRegStr HKCU "${UNINSTKEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
	WriteRegDWORD HKCU "${UNINSTKEY}" "NoModify" 1
	WriteRegDWORD HKCU "${UNINSTKEY}" "NoRepair" 1
	WriteRegStr HKCU "${INSTALLKEY}" "InstallDir" "$INSTDIR"
SectionEnd

Section "Uninstall"
	; Stop the daemon before touching its files: a running exe cannot be
	; replaced, and a force-killed daemon would leave the engine behind.
	nsExec::Exec 'taskkill /F /IM "${EXENAME}"'
	Pop $0
	nsExec::Exec 'taskkill /F /IM "${ENGINENAME}"'
	Pop $0

	Delete "$SMPROGRAMS\${APPNAME}.lnk"
	Delete "$INSTDIR\${EXENAME}"
	Delete "$INSTDIR\${ENGINENAME}"
	Delete "$INSTDIR\config.toml"
	Delete "$INSTDIR\${ICONNAME}"
	Delete "$INSTDIR\LICENSE.txt"
	Delete "$INSTDIR\uninstall.exe"
	RMDir "$INSTDIR"

	; The autostart value the daemon wrote at first start, so no dangling
	; entry survives an uninstall.
	DeleteRegValue HKCU "${RUNKEY}" "${RUNVALUE}"
	DeleteRegKey HKCU "${UNINSTKEY}"
	DeleteRegKey HKCU "${INSTALLKEY}"

	; The data dir holds the device pairing and the engine cache; removing
	; it un-pairs the device and drops cached torrents.
	RMDir /r "$LOCALAPPDATA\${APPNAME}"
SectionEnd
