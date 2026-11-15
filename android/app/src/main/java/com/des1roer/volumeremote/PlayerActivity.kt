package com.des1roer.volumeremote

import android.content.Context
import android.media.AudioManager
import android.os.Bundle
import android.os.SystemClock
import android.view.KeyEvent
import android.view.WindowManager
import androidx.activity.ComponentActivity
import androidx.annotation.OptIn
import androidx.activity.compose.setContent
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.view.WindowCompat
import androidx.core.view.WindowInsetsCompat
import androidx.core.view.WindowInsetsControllerCompat
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.lifecycleScope
import androidx.lifecycle.repeatOnLifecycle
import androidx.media3.common.MediaItem
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.DefaultRenderersFactory
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.ui.AspectRatioFrameLayout
import androidx.media3.ui.PlayerView
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

private const val SYNC_INTERVAL_MS = 1_000L
private const val CONTROLS_HIDE_MS = 4_000L

@OptIn(UnstableApi::class)
class PlayerActivity : ComponentActivity() {
    private lateinit var player: ExoPlayer
    private var currentFile: String? = null
    private var lastSeekAt = 0L
    private var playerError by mutableStateOf<String?>(null)
    private var muted by mutableStateOf(true)
    private val prefs by lazy { getSharedPreferences("remote", Context.MODE_PRIVATE) }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        WindowCompat.setDecorFitsSystemWindows(window, false)
        WindowInsetsControllerCompat(window, window.decorView).apply {
            hide(WindowInsetsCompat.Type.systemBars())
            systemBarsBehavior = WindowInsetsControllerCompat.BEHAVIOR_SHOW_TRANSIENT_BARS_BY_SWIPE
        }

        muted = prefs.getBoolean("videoMuted", true)
        val renderers = DefaultRenderersFactory(this)
            .setEnableDecoderFallback(true)
            .setExtensionRendererMode(DefaultRenderersFactory.EXTENSION_RENDERER_MODE_ON)
        player = ExoPlayer.Builder(this, renderers).build()
        player.volume = if (muted) 0f else 1f
        player.addListener(object : Player.Listener {
            override fun onPlayerError(error: PlaybackException) {
                playerError = error.errorCodeName + (error.cause?.message?.let { ": $it" } ?: "")
                currentFile = null
            }

            override fun onPlaybackStateChanged(playbackState: Int) {
                if (playbackState == Player.STATE_READY) {
                    playerError = null
                    sync()
                }
            }
        })

        setContent {
            val ui by Remote.state.collectAsState()
            RemoteTheme {
                PlayerScreen(
                    player = player,
                    ui = ui,
                    error = playerError,
                    muted = muted,
                    onToggleMute = { applyMuted(!muted) },
                    onClose = ::finish,
                )
            }
        }

        lifecycleScope.launch {
            repeatOnLifecycle(Lifecycle.State.STARTED) {
                launch { Remote.state.collect { follow(it) } }
                while (true) {
                    delay(SYNC_INTERVAL_MS)
                    sync()
                }
            }
        }
    }

    override fun onStart() {
        super.onStart()
        Remote.acquire(ui = true)
    }

    override fun onStop() {
        player.pause()
        Remote.release()
        super.onStop()
    }

    override fun onKeyDown(keyCode: Int, event: KeyEvent): Boolean {
        val direction = when (keyCode) {
            KeyEvent.KEYCODE_VOLUME_UP -> AudioManager.ADJUST_RAISE
            KeyEvent.KEYCODE_VOLUME_DOWN -> AudioManager.ADJUST_LOWER
            else -> return super.onKeyDown(keyCode, event)
        }
        if (muted && direction == AudioManager.ADJUST_RAISE) applyMuted(false)
        getSystemService(AudioManager::class.java)
            .adjustStreamVolume(AudioManager.STREAM_MUSIC, direction, AudioManager.FLAG_SHOW_UI)
        return true
    }

    private fun applyMuted(value: Boolean) {
        muted = value
        player.volume = if (value) 0f else 1f
        prefs.edit().putBoolean("videoMuted", value).apply()
    }

    override fun onDestroy() {
        player.release()
        super.onDestroy()
    }

    private fun follow(ui: UiState) {
        val status = ui.status ?: return
        if (!status.mpcOnline || status.file.isEmpty()) {
            if (currentFile != null) {
                player.stop()
                player.clearMediaItems()
                currentFile = null
            }
            return
        }
        val now = SystemClock.elapsedRealtime()
        if (status.file != currentFile) {
            currentFile = status.file
            player.setMediaItem(MediaItem.fromUri(mediaUrl(ui.server, status.file)), status.positionAt(now))
            player.prepare()
            lastSeekAt = now
        }
        player.playWhenReady = status.playing
        sync()
    }

    private fun sync() {
        val status = Remote.state.value.status ?: return
        if (status.file != currentFile || player.playbackState != Player.STATE_READY) return
        val now = SystemClock.elapsedRealtime()
        when (val action = syncAction(status.positionAt(now), player.currentPosition, status.playing, now, lastSeekAt)) {
            is SyncAction.Seek -> {
                player.seekTo(action.positionMs)
                player.setPlaybackSpeed(1f)
                lastSeekAt = now
            }
            is SyncAction.Speed -> if (player.playbackParameters.speed != action.speed) player.setPlaybackSpeed(action.speed)
        }
    }
}

@Composable
@OptIn(UnstableApi::class)
private fun PlayerScreen(
    player: ExoPlayer,
    ui: UiState,
    error: String?,
    muted: Boolean,
    onToggleMute: () -> Unit,
    onClose: () -> Unit,
) {
    var controlsVisible by remember { mutableStateOf(true) }
    var lastTouch by remember { mutableStateOf(0) }
    LaunchedEffect(controlsVisible, lastTouch) {
        if (controlsVisible) {
            delay(CONTROLS_HIDE_MS)
            controlsVisible = false
        }
    }
    val status = ui.status
    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Color.Black)
            .clickable(interactionSource = remember { MutableInteractionSource() }, indication = null) {
                controlsVisible = !controlsVisible
                lastTouch++
            },
    ) {
        AndroidView(
            factory = { context ->
                PlayerView(context).apply {
                    useController = false
                    resizeMode = AspectRatioFrameLayout.RESIZE_MODE_FIT
                    this.player = player
                }
            },
            modifier = Modifier.fillMaxSize(),
        )

        val message = when {
            status == null -> "Подключение к ПК…"
            !status.mpcOnline -> "MPC-HC не запущен"
            status.file.isEmpty() -> "В MPC-HC ничего не открыто"
            error != null -> "Не удалось воспроизвести: $error"
            else -> null
        }
        if (message != null) {
            Text(message, color = Color.White, fontSize = 20.sp, modifier = Modifier.align(Alignment.Center).padding(24.dp))
        }

        if (controlsVisible) {
            Column(
                modifier = Modifier
                    .align(Alignment.BottomCenter)
                    .fillMaxWidth()
                    .background(Color.Black.copy(alpha = 0.6f))
                    .padding(16.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                if (status != null && status.file.isNotEmpty()) {
                    Text(status.file, color = Color.White, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    Text(
                        "Осталось ${formatTime(status.remainingAt(SystemClock.elapsedRealtime()))}",
                        color = Color.White.copy(alpha = 0.8f),
                    )
                }
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    Button(onClick = { Remote.playPause(); lastTouch++ }) {
                        Text(if (status?.playing == true) "Пауза" else "Старт")
                    }
                    FilledTonalButton(onClick = { onToggleMute(); lastTouch++ }) {
                        Text(if (muted) "Звук на планшете: выкл" else "Звук на планшете: вкл")
                    }
                    FilledTonalButton(onClick = onClose) { Text("Закрыть") }
                }
            }
        }
    }
}
