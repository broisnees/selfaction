package dev.selfaction.hello;

import android.app.Activity;
import android.os.Bundle;
import android.widget.TextView;

/** Shows one line of text; it exists to prove the build works. */
public class MainActivity extends Activity {
    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        TextView text = new TextView(this);
        text.setText("Hello from selfaction");
        text.setPadding(48, 48, 48, 48);
        setContentView(text);
    }
}
