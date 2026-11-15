import * as hmUI from "@zos/ui";
import { px } from "@zos/utils";
import { BasePage } from "@zeppos/zml/base-page";
import { formatTime, positionAt } from "../shared/remote";

const POLL_MS = 2000;
const TICK_MS = 1000;
const WHITE = 0xffffff;
const GREY = 0x9e9e9e;
const ERROR = 0xff6e6e;
const BUTTON = 0x333333;
const BUTTON_PRESSED = 0x555555;
const ACCENT = 0xf5bd82;
const ACCENT_PRESSED = 0xc99a68;

Page(
  BasePage({
    state: {
      status: null,
      receivedAt: 0,
      error: null,
      busy: false,
      timers: [],
    },

    build() {
      this.title = hmUI.createWidget(hmUI.widget.TEXT, {
        x: px(90),
        y: px(36),
        w: px(300),
        h: px(64),
        color: GREY,
        text_size: px(22),
        align_h: hmUI.align.CENTER_H,
        align_v: hmUI.align.CENTER_V,
        text_style: hmUI.text_style.WRAP,
        text: "Подключение…",
      });
      this.remaining = hmUI.createWidget(hmUI.widget.TEXT, {
        x: px(40),
        y: px(100),
        w: px(400),
        h: px(84),
        color: WHITE,
        text_size: px(72),
        align_h: hmUI.align.CENTER_H,
        align_v: hmUI.align.CENTER_V,
        text: "--:--",
      });
      this.position = hmUI.createWidget(hmUI.widget.TEXT, {
        x: px(60),
        y: px(184),
        w: px(360),
        h: px(36),
        color: GREY,
        text_size: px(24),
        align_h: hmUI.align.CENTER_H,
        align_v: hmUI.align.CENTER_V,
        text: "",
      });

      this.button(px(52), px(230), px(116), px(90), "−10 с", () => this.send("seek", { offsetMs: -10000 }));
      this.playButton = this.button(px(176), px(230), px(128), px(90), "Старт", () => this.send("playpause"), true);
      this.button(px(312), px(230), px(116), px(90), "+10 с", () => this.send("seek", { offsetMs: 10000 }));

      this.button(px(84), px(330), px(100), px(66), "Пред.", () => this.send("prev"));
      this.button(px(192), px(330), px(96), px(66), "F5", () => this.send("overlay"));
      this.button(px(296), px(330), px(100), px(66), "След.", () => this.send("next"));

      this.button(px(134), px(404), px(64), px(52), "−", () => this.send("volumeDown"));
      this.volume = hmUI.createWidget(hmUI.widget.TEXT, {
        x: px(198),
        y: px(404),
        w: px(84),
        h: px(52),
        color: WHITE,
        text_size: px(26),
        align_h: hmUI.align.CENTER_H,
        align_v: hmUI.align.CENTER_V,
        text: "ПК",
      });
      this.button(px(282), px(404), px(64), px(52), "+", () => this.send("volumeUp"));

      this.poll();
      this.state.timers.push(setInterval(() => this.poll(), POLL_MS));
      this.state.timers.push(setInterval(() => this.render(), TICK_MS));
    },

    onDestroy() {
      this.state.timers.forEach((id) => clearInterval(id));
      this.state.timers = [];
    },

    button(x, y, w, h, text, onClick, accent = false) {
      return hmUI.createWidget(hmUI.widget.BUTTON, {
        x,
        y,
        w,
        h,
        radius: Math.floor(h / 2),
        text,
        text_size: px(28),
        color: accent ? 0x000000 : WHITE,
        normal_color: accent ? ACCENT : BUTTON,
        press_color: accent ? ACCENT_PRESSED : BUTTON_PRESSED,
        click_func: onClick,
      });
    },

    poll() {
      if (this.state.busy) return;
      this.send("status");
    },

    send(action, params = {}) {
      if (action === "status") this.state.busy = true;
      this.request({ method: action, params })
        .then((result) => {
          if (result && result.status) {
            this.state.status = result.status;
            this.state.receivedAt = Date.now();
            this.state.error = null;
          } else {
            this.state.error = (result && result.error) || "нет ответа";
          }
        })
        .catch((e) => {
          this.state.error = String((e && e.message) || e || "нет связи с телефоном");
        })
        .finally(() => {
          if (action === "status") this.state.busy = false;
          this.render();
        });
    },

    render() {
      const { status, receivedAt, error } = this.state;
      if (error) {
        this.title.setProperty(hmUI.prop.MORE, { text: "Нет связи с ПК: " + error, color: ERROR });
      } else if (!status) {
        this.title.setProperty(hmUI.prop.MORE, { text: "Подключение…", color: GREY });
      }
      if (!status) return;

      const active = status.online && status.file !== "";
      if (!error) {
        const title = !status.online ? "MPC-HC не запущен" : status.file || "Ничего не открыто";
        this.title.setProperty(hmUI.prop.MORE, { text: title, color: GREY });
      }
      const position = positionAt(status, receivedAt, Date.now());
      this.remaining.setProperty(hmUI.prop.TEXT, active ? formatTime(status.durationMs - position) : "--:--");
      this.position.setProperty(
        hmUI.prop.TEXT,
        active ? formatTime(position) + " / " + formatTime(status.durationMs) : ""
      );
      this.playButton.setProperty(hmUI.prop.TEXT, status.state === "playing" ? "Пауза" : "Старт");
      this.volume.setProperty(
        hmUI.prop.TEXT,
        status.volume == null ? "ПК" : status.muted ? "MUTE" : status.volume + "%"
      );
    },
  })
);
