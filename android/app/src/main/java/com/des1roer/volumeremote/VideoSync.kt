package com.des1roer.volumeremote

import java.net.URLEncoder
import kotlin.math.abs

const val SEEK_DRIFT_MS = 1_500L
const val SEEK_COOLDOWN_MS = 5_000L
const val SEEK_LEAD_MS = 700L
const val SPEED_DRIFT_MS = 150L

sealed interface SyncAction {
    data class Seek(val positionMs: Long) : SyncAction
    data class Speed(val speed: Float) : SyncAction
}

fun syncAction(expectedMs: Long, actualMs: Long, playing: Boolean, nowMs: Long, lastSeekAtMs: Long): SyncAction {
    val drift = expectedMs - actualMs
    if (abs(drift) > SEEK_DRIFT_MS && nowMs - lastSeekAtMs >= SEEK_COOLDOWN_MS) {
        return SyncAction.Seek(if (playing) expectedMs + SEEK_LEAD_MS else expectedMs)
    }
    if (!playing) return SyncAction.Speed(1f)
    return SyncAction.Speed(
        when {
            drift > SPEED_DRIFT_MS -> 1.05f
            drift < -SPEED_DRIFT_MS -> 0.95f
            else -> 1f
        },
    )
}

fun mediaUrl(server: String, file: String): String =
    normalizeServer(server) + "/media?file=" + URLEncoder.encode(file, Charsets.UTF_8).replace("+", "%20")
