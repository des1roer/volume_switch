import { DEFAULT_SERVER } from "../shared/remote";

AppSettingsPage({
  build(props) {
    const server = props.settingsStorage.getItem("server") || DEFAULT_SERVER;
    return View({ style: { padding: "12px 20px" } }, [
      TextInput({
        label: "Адрес ПК (volume_switch)",
        value: server,
        placeholder: DEFAULT_SERVER,
        onChange: (value) => props.settingsStorage.setItem("server", String(value).trim()),
      }),
      Text(
        { paragraph: true, style: { fontSize: "12px", color: "#888" } },
        "хост:порт, например " + DEFAULT_SERVER + ". На ПК в .env volume_switch должен быть задан API_LISTEN."
      ),
    ]);
  },
});
