package com.des1roer.volumeremote

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
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
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp

@Composable
fun RemoteScreen(ui: UiState) {
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

            Playback(status)

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

            VolumeControl(
                title = "Громкость ПК",
                percent = status?.volumePercent,
                muted = status?.muted == true,
                enabled = status?.volumePercent != null,
                hint = "Кнопки громкости телефона тоже меняют громкость ПК",
                onStep = { Remote.volume(up = it) },
            )

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
private fun Playback(status: Status?) {
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
        Text(
            text = "Осталось",
            style = MaterialTheme.typography.labelLarge,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Text(
            text = if (active) formatTime(status!!.remainingMs) else "--:--",
            fontSize = 64.sp,
            fontWeight = FontWeight.Bold,
        )
        val progress = if (active && status!!.durationMs > 0) {
            (status.positionMs.toFloat() / status.durationMs).coerceIn(0f, 1f)
        } else {
            0f
        }
        LinearProgressIndicator(progress = { progress }, modifier = Modifier.fillMaxWidth())
        Row {
            Text(if (active) formatTime(status!!.positionMs) else "")
            Spacer(Modifier.weight(1f))
            Text(if (active) formatTime(status!!.durationMs) else "")
        }
    }
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
