type PermissionQuery = (descriptor: PermissionDescriptor) => Promise<Pick<PermissionStatus, 'state'>>

export async function isClipboardReadGranted(
  queryPermission: PermissionQuery = (descriptor) => navigator.permissions.query(descriptor),
): Promise<boolean> {
  try {
    const permission = await queryPermission({ name: 'clipboard-read' as PermissionName })
    return permission.state === 'granted'
  } catch {
    return false
  }
}
