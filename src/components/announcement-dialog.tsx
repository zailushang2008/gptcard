import { useStorefrontI18n } from '@/lib/storefront-i18n'
import { useRef, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { announcementApplies, announcementDay, announcementHiddenToday, hideAnnouncementToday, type Announcement, type AnnouncementSettings } from '../../shared/announcements'
import './announcements.css'

export function AnnouncementDialog({ announcement, open, onClose, hideToday, onHideToday }: { announcement: Announcement; open: boolean; onClose: () => void; hideToday?: boolean; onHideToday?: (checked: boolean) => void }) {
  const { t } = useStorefrontI18n()
  const titleRef = useRef<HTMLHeadingElement>(null)
  return <Dialog open={open} onOpenChange={value => { if (!value) onClose() }}><DialogContent className="announcement-dialog motion-dialog" initialFocus={titleRef}>
    <DialogHeader><DialogTitle ref={titleRef} tabIndex={-1}>{announcement.title || t('站点公告')}</DialogTitle></DialogHeader>
    <DialogDescription className="announcement-content">{announcement.content}</DialogDescription>
    <div className="announcement-footer">
      {onHideToday && <label className="announcement-checkbox"><Checkbox checked={hideToday} onCheckedChange={onHideToday} /><span>{t('今天不再显示')}</span></label>}
      <Button onClick={onClose}>{t('我知道了')}</Button>
    </div>
  </DialogContent></Dialog>
}

function hiddenToday(shop: string) {
  try { return announcementHiddenToday(localStorage, shop) } catch { return false }
}

export function StoreAnnouncement({ announcement, shop, page, blocked = false }: { announcement?: AnnouncementSettings | null; shop?: string; page: string; blocked?: boolean }) {
  const [dismissed, setDismissed] = useState<string[]>([])
  const [hideToday, setHideToday] = useState(false)
  const scope = shop || 'platform'
  const key = `${scope}:${announcementDay()}:${announcement?.version}`
  const open = Boolean(announcement && announcementApplies(announcement, page) && !blocked && !dismissed.includes(key) && !hiddenToday(scope))
  function close() {
    if (hideToday) {
      try { hideAnnouncementToday(localStorage, scope) } catch { /* Private browsing may block access to storage. */ }
    }
    setDismissed(previous => [...previous, key]); setHideToday(false)
  }
  return announcement ? <AnnouncementDialog announcement={announcement} open={open} onClose={close} hideToday={hideToday} onHideToday={setHideToday} /> : null
}
