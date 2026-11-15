package com.des1roer.volumeremote

import org.junit.Assert.assertEquals
import org.junit.Test

class VideoSyncTest {
    @Test
    fun inSync() {
        assertEquals(SyncAction.Speed(1f), syncAction(60_000, 60_100, playing = true, nowMs = 100_000, lastSeekAtMs = 0))
        assertEquals(SyncAction.Speed(1f), syncAction(60_000, 60_000, playing = false, nowMs = 100_000, lastSeekAtMs = 0))
    }

    @Test
    fun smallDriftAdjustsSpeed() {
        assertEquals(SyncAction.Speed(1.05f), syncAction(60_000, 59_500, playing = true, nowMs = 100_000, lastSeekAtMs = 0))
        assertEquals(SyncAction.Speed(0.95f), syncAction(60_000, 60_500, playing = true, nowMs = 100_000, lastSeekAtMs = 0))
        assertEquals(SyncAction.Speed(1f), syncAction(60_000, 59_000, playing = false, nowMs = 100_000, lastSeekAtMs = 0))
    }

    @Test
    fun bigDriftSeeks() {
        assertEquals(SyncAction.Seek(60_000 + SEEK_LEAD_MS), syncAction(60_000, 50_000, playing = true, nowMs = 100_000, lastSeekAtMs = 0))
        assertEquals(SyncAction.Seek(60_000), syncAction(60_000, 70_000, playing = false, nowMs = 100_000, lastSeekAtMs = 0))
    }

    @Test
    fun seekCooldown() {
        val action = syncAction(60_000, 50_000, playing = true, nowMs = 100_000, lastSeekAtMs = 97_000)
        assertEquals(SyncAction.Speed(1.05f), action)
    }

    @Test
    fun mediaUrl() {
        assertEquals(
            "http://192.168.3.2:7778/media?file=%D0%A1%D0%B5%D1%80%D0%B8%D1%8F%2001%20%26%20b.mkv",
            mediaUrl("192.168.3.2:7778", "Серия 01 & b.mkv"),
        )
    }
}
