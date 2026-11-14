package com.des1roer.volumeremote

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.ServiceInfo
import android.media.MediaMetadata
import android.media.VolumeProvider
import android.media.session.MediaSession
import android.media.session.PlaybackState
import android.os.Bundle
import android.os.IBinder
import android.os.PowerManager
import android.os.SystemClock
import kotlinx.coroutines.MainScope
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch
import kotlin.math.abs

private const val CHANNEL_ID = "remote"
private const val NOTIFICATION_ID = 1
private const val ACTION_VOLUME_DOWN = "volume_down"
private const val ACTION_VOLUME_UP = "volume_up"
private const val IDLE_STOP_MS = 30 * 60 * 1000L

class RemoteService : Service() {
    private val scope = MainScope()
    private lateinit var session: MediaSession
    private var idleSince = SystemClock.elapsedRealtime()

    private val screenReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) {
            when (intent.action) {
                Intent.ACTION_SCREEN_ON -> Remote.setScreenOn(true)
                Intent.ACTION_SCREEN_OFF -> Remote.setScreenOn(false)
            }
        }
    }

    private val volumeProvider = object : VolumeProvider(VOLUME_CONTROL_ABSOLUTE, 100, 0) {
        override fun onAdjustVolume(direction: Int) {
            if (direction != 0) Remote.volume(up = direction > 0)
        }

        override fun onSetVolumeTo(volume: Int) {
            val diff = volume - currentVolume
            if (diff != 0) Remote.volume(up = diff > 0, step = abs(diff))
        }
    }

    private val callback = object : MediaSession.Callback() {
        override fun onPlay() {
            if (Remote.state.value.status?.playing != true) Remote.playPause()
        }

        override fun onPause() {
            if (Remote.state.value.status?.playing == true) Remote.playPause()
        }

        override fun onSkipToNext() = Remote.next()

        override fun onSkipToPrevious() = Remote.prev()

        override fun onCustomAction(action: String, extras: Bundle?) {
            when (action) {
                ACTION_VOLUME_DOWN -> Remote.volume(up = false)
                ACTION_VOLUME_UP -> Remote.volume(up = true)
            }
        }
    }

    override fun onCreate() {
        super.onCreate()
        Remote.init(this)
        getSystemService(NotificationManager::class.java).createNotificationChannel(
            NotificationChannel(CHANNEL_ID, getString(R.string.app_name), NotificationManager.IMPORTANCE_LOW),
        )
        session = MediaSession(this, "VolumeRemote").apply {
            setCallback(callback)
            setSessionActivity(openAppIntent())
            setPlaybackToRemote(volumeProvider)
            isActive = true
        }
        render(Remote.state.value)
        startForeground(NOTIFICATION_ID, buildNotification(), ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PLAYBACK)
        Remote.setScreenOn(getSystemService(PowerManager::class.java).isInteractive)
        registerReceiver(
            screenReceiver,
            IntentFilter().apply {
                addAction(Intent.ACTION_SCREEN_ON)
                addAction(Intent.ACTION_SCREEN_OFF)
            },
            RECEIVER_NOT_EXPORTED,
        )
        Remote.acquire()
        scope.launch { Remote.state.collect { render(it) } }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int = START_NOT_STICKY

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onDestroy() {
        unregisterReceiver(screenReceiver)
        Remote.setScreenOn(true)
        scope.cancel()
        Remote.release()
        session.release()
        super.onDestroy()
    }

    private fun render(ui: UiState) {
        val status = ui.status
        val active = status != null && status.mpcOnline && status.file.isNotEmpty()
        val playing = status?.playing == true

        val title = when {
            status == null -> "Подключение…"
            !status.mpcOnline -> "MPC-HC не запущен"
            status.file.isEmpty() -> "Ничего не открыто"
            else -> status.file
        }
        val subtitle = listOfNotNull(
            if (ui.error != null) "Нет связи с ПК" else null,
            if (active) "Осталось ${formatTime(status!!.remainingMs)}" else null,
            status?.volumePercent?.let { "ПК $it%" },
        ).joinToString(" · ")

        session.setMetadata(
            MediaMetadata.Builder()
                .putString(MediaMetadata.METADATA_KEY_TITLE, title)
                .putString(MediaMetadata.METADATA_KEY_ARTIST, subtitle)
                .putLong(MediaMetadata.METADATA_KEY_DURATION, if (active) status!!.durationMs else 0)
                .build(),
        )

        var actions = PlaybackState.ACTION_PLAY or PlaybackState.ACTION_PAUSE or PlaybackState.ACTION_PLAY_PAUSE
        if (status?.mpcOnline == true) {
            actions = actions or PlaybackState.ACTION_SKIP_TO_NEXT or PlaybackState.ACTION_SKIP_TO_PREVIOUS
        }
        session.setPlaybackState(
            PlaybackState.Builder()
                .setActions(actions)
                .setState(
                    if (playing) PlaybackState.STATE_PLAYING else PlaybackState.STATE_PAUSED,
                    if (active) status!!.positionMs else 0,
                    if (playing) 1f else 0f,
                    SystemClock.elapsedRealtime(),
                )
                .addCustomAction(ACTION_VOLUME_DOWN, "Тише (ПК)", R.drawable.ic_volume_down)
                .addCustomAction(ACTION_VOLUME_UP, "Громче (ПК)", R.drawable.ic_volume_up)
                .build(),
        )

        status?.volumePercent?.let { volumeProvider.currentVolume = it }

        val now = SystemClock.elapsedRealtime()
        if (playing) {
            idleSince = now
        } else if (now - idleSince > IDLE_STOP_MS) {
            stopSelf()
        }
    }

    private fun buildNotification(): Notification =
        Notification.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(getString(R.string.app_name))
            .setContentIntent(openAppIntent())
            .setVisibility(Notification.VISIBILITY_PUBLIC)
            .setOngoing(true)
            .setStyle(Notification.MediaStyle().setMediaSession(session.sessionToken))
            .build()

    private fun openAppIntent(): PendingIntent = PendingIntent.getActivity(
        this,
        0,
        Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
        PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
    )
}
