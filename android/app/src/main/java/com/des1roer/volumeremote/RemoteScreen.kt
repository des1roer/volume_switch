package com.des1roer.volumeremote

import android.content.Intent
import android.os.SystemClock
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Slider
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.produceState
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.lifecycle.repeatOnLifecycle
import kotlinx.coroutines.delay

@Composable
fun RemoteScreen(ui: UiState, onExit: () -> Unit) {
    val status = ui.status
    Scaffold { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding)
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 20.dp, vertical = 16.dp),
            verticalArrangement = Arrangement.spacedBy(16.dp),
        ) {
            ServerField(ui.server, Remote::setServer)

            ui.error?.let {
                Text(
                    text = "Нет связи с ПК: $it",
                    color = MaterialTheme.colorScheme.error,
                    style = MaterialTheme.typography.bodyMedium,
                )
            }

            Playback(status, onExit)

            Button(
                onClick = Remote::playPause,
                modifier = Modifier
                    .fillMaxWidth()
                    .height(96.dp),
            ) {
                Text(
                    text = if (status?.playing == true) "Пауза" else "Старт",
                    fontSize = 32.sp,
                )
            }

            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                val online = status?.mpcOnline == true
                OutlinedButton(
                    onClick = Remote::prev,
                    enabled = online,
                    modifier = Modifier
                        .weight(1f)
                        .height(64.dp),
                ) {
                    Text("⏮ Предыдущая")
                }
                Button(
                    onClick = Remote::next,
                    enabled = online,
                    modifier = Modifier
                        .weight(1f)
                        .height(64.dp),
                ) {
                    Text("Следующая ⏭")
                }
            }

            val context = LocalContext.current
            OutlinedButton(
                onClick = { context.startActivity(Intent(context, PlayerActivity::class.java)) },
                enabled = status?.mpcOnline == true && status.file.isNotEmpty(),
                modifier = Modifier
                    .fillMaxWidth()
                    .height(56.dp),
            ) {
                Text("Смотреть на этом устройстве")
            }

            OutlinedButton(
                onClick = Remote::toggleOverlay,
                enabled = status != null,
                modifier = Modifier
                    .fillMaxWidth()
                    .height(56.dp),
            ) {
                Text("Статус-бар на ПК (F5)")
            }

            VolumeControl(
                title = "Громкость ПК",
                percent = status?.volumePercent,
                muted = status?.muted == true,
                enabled = status?.volumePercent != null,
                hint = null,
                onStep = { Remote.volume(up = it) },
            )

            val volumeKeysToPc by Remote.volumeKeysToPc.collectAsState()
            Row(verticalAlignment = Alignment.CenterVertically) {
                Column(modifier = Modifier.weight(1f)) {
                    Text("Кнопки громкости управляют ПК", style = MaterialTheme.typography.bodyLarge)
                    Text(
                        text = if (volumeKeysToPc) {
                            "Кнопки громкости этого устройства меняют громкость ПК"
                        } else {
                            "Кнопки громкости меняют громкость этого устройства"
                        },
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                Switch(checked = volumeKeysToPc, onCheckedChange = Remote::setVolumeKeysToPc)
            }

            val mpcOnline = status?.mpcOnline == true
            VolumeControl(
                title = "Громкость MPC",
                percent = if (mpcOnline) status?.mpcVolume else null,
                muted = status?.mpcMuted == true,
                enabled = mpcOnline,
                hint = null,
                onStep = Remote::mpcVolume,
            )
        }
    }
}

@Composable
private fun ServerField(server: String, onSave: (String) -> Unit) {
    var text by rememberSaveable(server) { mutableStateOf(server) }
    val focus = LocalFocusManager.current
    val save = {
        onSave(text)
        focus.clearFocus()
    }
    Row(verticalAlignment = Alignment.CenterVertically) {
        OutlinedTextField(
            value = text,
            onValueChange = { text = it },
            label = { Text("Адрес ПК (volume_switch)") },
            singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri, imeAction = ImeAction.Done),
            keyboardActions = KeyboardActions(onDone = { save() }),
            modifier = Modifier.weight(1f),
        )
        if (text.trim() != server) {
            TextButton(onClick = save) { Text("OK") }
        }
    }
}

@Composable
private fun Playback(status: Status?, onExit: () -> Unit) {
    val title = when {
        status == null -> "Подключение…"
        !status.mpcOnline -> "MPC-HC не запущен"
        status.file.isEmpty() -> "Ничего не открыто"
        else -> status.file
    }
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text(
            text = title,
            style = MaterialTheme.typography.titleMedium,
            maxLines = 3,
            overflow = TextOverflow.Ellipsis,
        )
        val active = status != null && status.mpcOnline && status.file.isNotEmpty()
        val now = rememberPlaybackClock(status?.takeIf { it.playing })
        var dragMs by remember { mutableStateOf<Long?>(null) }
        val position = if (active) dragMs ?: status!!.positionAt(now) else 0
        val duration = if (active) status!!.durationMs else 0
        Text(
            text = "Осталось",
            style = MaterialTheme.typography.labelLarge,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(
                text = if (active) formatTime(duration - position) else "--:--",
                fontSize = 64.sp,
                fontWeight = FontWeight.Bold,
                modifier = Modifier.weight(1f),
            )
            OutlinedButton(
                onClick = onExit,
                modifier = Modifier.height(56.dp),
            ) {
                Text("Выйти")
            }
        }
        SeekSlider(
            positionMs = position,
            durationMs = duration,
            onDrag = { dragMs = it },
            onDragEnd = {
                dragMs?.let(Remote::seekTo)
                dragMs = null
            },
        )
        Row {
            Text(if (active) formatTime(position) else "")
            Spacer(Modifier.weight(1f))
            Text(if (active) formatTime(duration) else "")
        }
        SeekButtons(enabled = active && duration > 0)
    }
}

@Composable
fun SeekSlider(
    positionMs: Long,
    durationMs: Long,
    onDrag: (Long) -> Unit,
    onDragEnd: () -> Unit,
    modifier: Modifier = Modifier,
) {
    Slider(
        value = positionMs.coerceIn(0, maxOf(durationMs, 0)).toFloat(),
        onValueChange = { onDrag(it.toLong()) },
        onValueChangeFinished = onDragEnd,
        valueRange = 0f..maxOf(durationMs, 1).toFloat(),
        enabled = durationMs > 0,
        modifier = modifier.fillMaxWidth(),
    )
}

private val SEEK_STEPS = listOf(-60_000L to "−1 мин", -10_000L to "−10 с", 10_000L to "+10 с", 60_000L to "+1 мин")

@Composable
fun SeekButtons(enabled: Boolean, modifier: Modifier = Modifier, onSeek: () -> Unit = {}) {
    Row(horizontalArrangement = Arrangement.spacedBy(8.dp), modifier = modifier) {
        for ((offset, label) in SEEK_STEPS) {
            FilledTonalButton(
                onClick = {
                    Remote.seekBy(offset)
                    onSeek()
                },
                enabled = enabled,
                contentPadding = PaddingValues(horizontal = 4.dp),
                modifier = Modifier
                    .weight(1f)
                    .height(56.dp),
            ) {
                Text(label, maxLines = 1)
            }
        }
    }
}

@Composable
fun rememberPlaybackClock(playing: Status?): Long {
    val lifecycle = LocalLifecycleOwner.current.lifecycle
    val now by produceState(SystemClock.elapsedRealtime(), playing, lifecycle) {
        value = SystemClock.elapsedRealtime()
        if (playing == null) return@produceState
        lifecycle.repeatOnLifecycle(Lifecycle.State.STARTED) {
            while (true) {
                value = SystemClock.elapsedRealtime()
                delay(1_000 - (value - playing.receivedAt).mod(1_000L))
            }
        }
    }
    return now
}

@Composable
private fun VolumeControl(
    title: String,
    percent: Int?,
    muted: Boolean,
    enabled: Boolean,
    hint: String?,
    onStep: (Boolean) -> Unit,
) {
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Text(
            text = title,
            style = MaterialTheme.typography.labelLarge,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Row(verticalAlignment = Alignment.CenterVertically) {
            FilledTonalButton(
                onClick = { onStep(false) },
                enabled = enabled,
                modifier = Modifier.size(width = 96.dp, height = 64.dp),
            ) {
                Text("−", fontSize = 32.sp)
            }
            Text(
                text = when {
                    percent == null -> "—"
                    muted -> "MUTE"
                    else -> "$percent%"
                },
                fontSize = 36.sp,
                fontWeight = FontWeight.Bold,
                textAlign = TextAlign.Center,
                modifier = Modifier.weight(1f),
            )
            FilledTonalButton(
                onClick = { onStep(true) },
                enabled = enabled,
                modifier = Modifier.size(width = 96.dp, height = 64.dp),
            ) {
                Text("+", fontSize = 32.sp)
            }
        }
        hint?.let {
            Text(
                text = it,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}
