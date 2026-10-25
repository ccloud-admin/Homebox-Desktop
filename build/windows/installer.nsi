; Homebox Desktop installer (NSIS 3)
Unicode true
ManifestDPIAware true

!ifndef VERSION
  !define VERSION "1.4.0"
!endif
!ifndef OUTFILE
  !define OUTFILE "HomeboxDesktop-Setup-${VERSION}.exe"
!endif

!define APPNAME   "Homebox Desktop"
!define EXENAME   "HomeboxDesktop.exe"
!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\HomeboxDesktop"
!define FWRULE    "Homebox Desktop (home network sharing)"

Name "${APPNAME}"
OutFile "${OUTFILE}"
InstallDir "$PROGRAMFILES64\${APPNAME}"
InstallDirRegKey HKLM "Software\HomeboxDesktop" "InstallDir"
RequestExecutionLevel admin
SetCompressor /SOLID lzma
BrandingText "${APPNAME} ${VERSION}"

VIProductVersion "${VERSION}.0"
VIAddVersionKey "ProductName" "${APPNAME}"
VIAddVersionKey "FileDescription" "${APPNAME} installer"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "LegalCopyright" "AGPL-3.0"

!include "MUI2.nsh"
!include "x64.nsh"
!include "LogicLib.nsh"

!define MUI_ICON "icon.ico"
!define MUI_UNICON "icon.ico"
!define MUI_ABORTWARNING
!define MUI_WELCOMEPAGE_TITLE "Install ${APPNAME}"
!define MUI_WELCOMEPAGE_TEXT "This installs ${APPNAME} ${VERSION}, an unofficial Windows app for Homebox, the open-source home inventory system. It is not made by or affiliated with the Homebox project.$\r$\n$\r$\nThe first time it opens, ${APPNAME} downloads the official Homebox engine from GitHub, so an internet connection is needed for that first start.$\r$\n$\r$\nYour inventory is stored privately on this PC, in your own user folder."
!define MUI_FINISHPAGE_RUN "$INSTDIR\${EXENAME}"
!define MUI_FINISHPAGE_RUN_TEXT "Open ${APPNAME} now"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_COMPONENTS
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

Function .onInit
  ${IfNot} ${RunningX64}
    MessageBox MB_ICONSTOP "${APPNAME} needs 64-bit Windows 10 or 11."
    Abort
  ${EndIf}
  SetRegView 64
FunctionEnd

Function un.onInit
  SetRegView 64
FunctionEnd

Section "${APPNAME} (required)" SecMain
  SectionIn RO
  SetShellVarContext all

  ; Close a running copy so its files can be replaced. The Homebox engine
  ; closes with it.
  nsExec::Exec 'taskkill /IM "${EXENAME}" /F'
  Sleep 800

  SetOutPath "$INSTDIR"
  File "../bin/${EXENAME}"
  File "icon.ico"
  File "../../LICENSE.txt"
  File "../../README.txt"

  CreateDirectory "$SMPROGRAMS\${APPNAME}"
  CreateShortcut "$SMPROGRAMS\${APPNAME}\${APPNAME}.lnk" "$INSTDIR\${EXENAME}" "" "$INSTDIR\icon.ico"
  CreateShortcut "$SMPROGRAMS\${APPNAME}\Uninstall ${APPNAME}.lnk" "$INSTDIR\Uninstall.exe"

  ; Allow network sharing on private (home) networks only. Nothing listens
  ; on the network unless sharing is switched on inside the app.
  nsExec::Exec 'netsh advfirewall firewall delete rule name="${FWRULE}"'
  nsExec::Exec 'netsh advfirewall firewall add rule name="${FWRULE}" dir=in action=allow program="$INSTDIR\${EXENAME}" enable=yes profile=private'

  WriteUninstaller "$INSTDIR\Uninstall.exe"
  WriteRegStr HKLM "Software\HomeboxDesktop" "InstallDir" "$INSTDIR"
  WriteRegStr HKLM "${UNINSTKEY}" "DisplayName" "${APPNAME}"
  WriteRegStr HKLM "${UNINSTKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKLM "${UNINSTKEY}" "Publisher" "Homebox Desktop"
  WriteRegStr HKLM "${UNINSTKEY}" "DisplayIcon" "$INSTDIR\icon.ico"
  WriteRegStr HKLM "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKLM "${UNINSTKEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegStr HKLM "${UNINSTKEY}" "URLInfoAbout" "https://homebox.software"
  WriteRegDWORD HKLM "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD HKLM "${UNINSTKEY}" "NoRepair" 1

  ; Homebox Desktop's window uses Microsoft Edge WebView2, which is part of
  ; Windows 11 and up-to-date Windows 10. Point to it if it is missing.
  ReadRegStr $0 HKLM "SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
  ReadRegStr $1 HKCU "Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
  ${If} $0 == ""
  ${AndIf} $1 == ""
    MessageBox MB_YESNO|MB_ICONINFORMATION "${APPNAME} needs the Microsoft Edge WebView2 Runtime, which doesn't appear to be installed.$\r$\n$\r$\nOpen Microsoft's download page now?" IDNO +2
      ExecShell "open" "https://developer.microsoft.com/microsoft-edge/webview2/#download-section"
  ${EndIf}
SectionEnd

Section "Desktop shortcut" SecDesktop
  SetShellVarContext all
  CreateShortcut "$DESKTOP\${APPNAME}.lnk" "$INSTDIR\${EXENAME}" "" "$INSTDIR\icon.ico"
SectionEnd

!insertmacro MUI_FUNCTION_DESCRIPTION_BEGIN
  !insertmacro MUI_DESCRIPTION_TEXT ${SecMain} "The ${APPNAME} app."
  !insertmacro MUI_DESCRIPTION_TEXT ${SecDesktop} "Put a ${APPNAME} shortcut on the desktop."
!insertmacro MUI_FUNCTION_DESCRIPTION_END

Section "Uninstall"
  SetShellVarContext all
  nsExec::Exec 'taskkill /IM "${EXENAME}" /F'
  Sleep 800

  Delete "$INSTDIR\${EXENAME}"
  Delete "$INSTDIR\icon.ico"
  Delete "$INSTDIR\LICENSE.txt"
  Delete "$INSTDIR\README.txt"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"

  Delete "$SMPROGRAMS\${APPNAME}\${APPNAME}.lnk"
  Delete "$SMPROGRAMS\${APPNAME}\Uninstall ${APPNAME}.lnk"
  RMDir "$SMPROGRAMS\${APPNAME}"
  Delete "$DESKTOP\${APPNAME}.lnk"

  nsExec::Exec 'netsh advfirewall firewall delete rule name="${FWRULE}"'
  DeleteRegKey HKLM "${UNINSTKEY}"
  DeleteRegKey HKLM "Software\HomeboxDesktop"

  ; Your inventory is kept unless you choose otherwise.
  SetShellVarContext current
  IfFileExists "$LOCALAPPDATA\HomeboxDesktop\*.*" 0 done
    MessageBox MB_YESNO|MB_ICONEXCLAMATION|MB_DEFBUTTON2 "Also delete your Homebox inventory from this PC?$\r$\n$\r$\nThis permanently removes every item, photo and document stored in Homebox Desktop for your Windows account. Choose No to keep them (recommended unless you have a backup)." IDNO done
      RMDir /r "$LOCALAPPDATA\HomeboxDesktop"
  done:
SectionEnd
